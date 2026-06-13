package lta

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

func TestSearchStopsByCodeUsesFilterAndAccountKey(t *testing.T) {
	httpClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if got := r.Header.Get("AccountKey"); got != "secret" {
			t.Errorf("AccountKey = %q, want secret", got)
		}
		if got := r.URL.Query().Get("BusStopCode"); got != "02049" {
			t.Errorf("BusStopCode = %q, want 02049", got)
		}
		return jsonResponse(map[string]any{
			"value": []BusStop{{
				BusStopCode: "02049",
				RoadName:    "Bras Basah Rd",
				Description: "Raffles Hotel",
			}},
		}), nil
	})}

	client := NewWithBaseURL("secret", "https://lta.test", httpClient)
	stops, err := client.SearchStops(context.Background(), "02049", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(stops) != 1 || stops[0].Description != "Raffles Hotel" {
		t.Fatalf("stops = %#v", stops)
	}
}

func TestArrivalsUsesV3EndpointAndFiltersService(t *testing.T) {
	httpClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/v3/BusArrival" {
			t.Errorf("path = %q, want /v3/BusArrival", r.URL.Path)
		}
		if r.URL.Query().Get("BusStopCode") != "02049" || r.URL.Query().Get("ServiceNo") != "36" {
			t.Errorf("query = %q", r.URL.RawQuery)
		}
		return jsonResponse(map[string]any{
			"Services": []ServiceArrival{{ServiceNo: "36"}},
		}), nil
	})}

	client := NewWithBaseURL("secret", "https://lta.test", httpClient)
	services, err := client.Arrivals(context.Background(), "02049", "36")
	if err != nil {
		t.Fatal(err)
	}
	if len(services) != 1 || services[0].ServiceNo != "36" {
		t.Fatalf("services = %#v", services)
	}
}

func TestServicesAtStopsUsesBusRoutesAndDeduplicatesServices(t *testing.T) {
	httpClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/BusRoutes" {
			t.Errorf("path = %q, want /BusRoutes", r.URL.Path)
		}
		if r.URL.Query().Get("$skip") != "0" {
			t.Errorf("$skip = %q, want 0", r.URL.Query().Get("$skip"))
		}
		return jsonResponse(map[string]any{
			"value": []BusRoute{
				{ServiceNo: "36", BusStopCode: "02049"},
				{ServiceNo: "36", BusStopCode: "02049"},
				{ServiceNo: "111", BusStopCode: "04167"},
				{ServiceNo: "7", BusStopCode: "01019"},
			},
		}), nil
	})}

	client := NewWithBaseURL("secret", "https://lta.test", httpClient)
	services, err := client.ServicesAtStops(context.Background(), []string{"02049", "04167"})
	if err != nil {
		t.Fatal(err)
	}
	if len(services["02049"]) != 1 || services["02049"][0] != "36" {
		t.Fatalf("02049 services = %#v", services["02049"])
	}
	if len(services["04167"]) != 1 || services["04167"][0] != "111" {
		t.Fatalf("04167 services = %#v", services["04167"])
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func jsonResponse(value any) *http.Response {
	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(value); err != nil {
		panic(err)
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     make(http.Header),
		Body:       io.NopCloser(bytes.NewReader(body.Bytes())),
	}
}
