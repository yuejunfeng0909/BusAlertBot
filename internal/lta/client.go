package lta

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const defaultBaseURL = "https://datamall2.mytransport.sg/ltaodataservice"

type BusStop struct {
	BusStopCode string  `json:"BusStopCode"`
	RoadName    string  `json:"RoadName"`
	Description string  `json:"Description"`
	Latitude    float64 `json:"Latitude"`
	Longitude   float64 `json:"Longitude"`
}

type Arrival struct {
	OriginCode       string `json:"OriginCode"`
	DestinationCode  string `json:"DestinationCode"`
	EstimatedArrival string `json:"EstimatedArrival"`
	Monitored        int    `json:"Monitored"`
	Load             string `json:"Load"`
	Feature          string `json:"Feature"`
	Type             string `json:"Type"`
}

type ServiceArrival struct {
	ServiceNo string  `json:"ServiceNo"`
	Operator  string  `json:"Operator"`
	NextBus   Arrival `json:"NextBus"`
	NextBus2  Arrival `json:"NextBus2"`
	NextBus3  Arrival `json:"NextBus3"`
}

type BusRoute struct {
	ServiceNo   string `json:"ServiceNo"`
	BusStopCode string `json:"BusStopCode"`
}

type Client struct {
	accountKey string
	baseURL    string
	http       *http.Client

	stopsMu       sync.RWMutex
	stops         []BusStop
	stopsLoadedAt time.Time

	routesMu       sync.RWMutex
	routes         []BusRoute
	routesLoadedAt time.Time
}

func New(accountKey string) *Client {
	return &Client{
		accountKey: accountKey,
		baseURL:    defaultBaseURL,
		http:       &http.Client{Timeout: 15 * time.Second},
	}
}

func NewWithBaseURL(accountKey, baseURL string, client *http.Client) *Client {
	return &Client{accountKey: accountKey, baseURL: strings.TrimRight(baseURL, "/"), http: client}
}

func (c *Client) SearchStops(ctx context.Context, query string, limit int) ([]BusStop, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}
	if limit < 1 {
		limit = 10
	}

	if isStopCode(query) {
		var response struct {
			Value []BusStop `json:"value"`
		}
		params := url.Values{"BusStopCode": {query}}
		if err := c.get(ctx, "/BusStops", params, &response); err != nil {
			return nil, err
		}
		if len(response.Value) > 0 {
			return response.Value[:min(limit, len(response.Value))], nil
		}
	}

	stops, err := c.allStops(ctx)
	if err != nil {
		return nil, err
	}
	needle := strings.ToLower(query)
	type match struct {
		stop BusStop
		rank int
	}
	var matches []match
	for _, stop := range stops {
		name := strings.ToLower(stop.Description)
		road := strings.ToLower(stop.RoadName)
		code := strings.ToLower(stop.BusStopCode)
		rank := 99
		switch {
		case name == needle || code == needle:
			rank = 0
		case strings.HasPrefix(name, needle):
			rank = 1
		case strings.Contains(name, needle):
			rank = 2
		case strings.HasPrefix(road, needle):
			rank = 3
		case strings.Contains(road, needle):
			rank = 4
		}
		if rank < 99 {
			matches = append(matches, match{stop: stop, rank: rank})
		}
	}
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].rank != matches[j].rank {
			return matches[i].rank < matches[j].rank
		}
		return matches[i].stop.Description < matches[j].stop.Description
	})
	result := make([]BusStop, 0, min(limit, len(matches)))
	for _, candidate := range matches[:min(limit, len(matches))] {
		result = append(result, candidate.stop)
	}
	return result, nil
}

func (c *Client) Arrivals(ctx context.Context, stopCode, serviceNo string) ([]ServiceArrival, error) {
	var response struct {
		Services []ServiceArrival `json:"Services"`
	}
	params := url.Values{"BusStopCode": {stopCode}}
	if serviceNo != "" {
		params.Set("ServiceNo", serviceNo)
	}
	if err := c.get(ctx, "/v3/BusArrival", params, &response); err != nil {
		return nil, err
	}
	return response.Services, nil
}

func (c *Client) ServicesAtStops(ctx context.Context, stopCodes []string) (map[string][]string, error) {
	routes, err := c.allRoutes(ctx)
	if err != nil {
		return nil, err
	}
	wanted := make(map[string]bool, len(stopCodes))
	for _, code := range stopCodes {
		wanted[code] = true
	}
	services := make(map[string][]string, len(stopCodes))
	seen := make(map[string]map[string]bool, len(stopCodes))
	for _, route := range routes {
		if !wanted[route.BusStopCode] {
			continue
		}
		if seen[route.BusStopCode] == nil {
			seen[route.BusStopCode] = make(map[string]bool)
		}
		serviceNo := strings.ToUpper(route.ServiceNo)
		if seen[route.BusStopCode][serviceNo] {
			continue
		}
		seen[route.BusStopCode][serviceNo] = true
		services[route.BusStopCode] = append(services[route.BusStopCode], serviceNo)
	}
	return services, nil
}

func (c *Client) allStops(ctx context.Context) ([]BusStop, error) {
	c.stopsMu.RLock()
	if len(c.stops) > 0 && time.Since(c.stopsLoadedAt) < 24*time.Hour {
		stops := append([]BusStop(nil), c.stops...)
		c.stopsMu.RUnlock()
		return stops, nil
	}
	c.stopsMu.RUnlock()

	c.stopsMu.Lock()
	defer c.stopsMu.Unlock()
	if len(c.stops) > 0 && time.Since(c.stopsLoadedAt) < 24*time.Hour {
		return append([]BusStop(nil), c.stops...), nil
	}

	var all []BusStop
	for skip := 0; ; skip += 500 {
		var response struct {
			Value []BusStop `json:"value"`
		}
		if err := c.get(ctx, "/BusStops", url.Values{"$skip": {strconv.Itoa(skip)}}, &response); err != nil {
			return nil, err
		}
		all = append(all, response.Value...)
		if len(response.Value) < 500 {
			break
		}
	}
	c.stops = all
	c.stopsLoadedAt = time.Now()
	return append([]BusStop(nil), all...), nil
}

func (c *Client) allRoutes(ctx context.Context) ([]BusRoute, error) {
	c.routesMu.RLock()
	if len(c.routes) > 0 && time.Since(c.routesLoadedAt) < 24*time.Hour {
		routes := append([]BusRoute(nil), c.routes...)
		c.routesMu.RUnlock()
		return routes, nil
	}
	c.routesMu.RUnlock()

	c.routesMu.Lock()
	defer c.routesMu.Unlock()
	if len(c.routes) > 0 && time.Since(c.routesLoadedAt) < 24*time.Hour {
		return append([]BusRoute(nil), c.routes...), nil
	}

	var all []BusRoute
	for skip := 0; ; skip += 500 {
		var response struct {
			Value []BusRoute `json:"value"`
		}
		if err := c.get(ctx, "/BusRoutes", url.Values{"$skip": {strconv.Itoa(skip)}}, &response); err != nil {
			return nil, err
		}
		all = append(all, response.Value...)
		if len(response.Value) < 500 {
			break
		}
	}
	c.routes = all
	c.routesLoadedAt = time.Now()
	return append([]BusRoute(nil), all...), nil
}

func (c *Client) get(ctx context.Context, path string, params url.Values, dst any) error {
	endpoint := c.baseURL + path
	if encoded := params.Encode(); encoded != "" {
		endpoint += "?" + encoded
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("create LTA request: %w", err)
	}
	req.Header.Set("AccountKey", c.accountKey)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("call LTA API: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("LTA API returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(dst); err != nil {
		return fmt.Errorf("decode LTA response: %w", err)
	}
	return nil
}

func isStopCode(value string) bool {
	if len(value) != 5 {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
