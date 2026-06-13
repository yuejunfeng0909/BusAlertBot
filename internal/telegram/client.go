package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const defaultBaseURL = "https://api.telegram.org"

type Chat struct {
	ID int64 `json:"id"`
}

type Message struct {
	MessageID int64  `json:"message_id"`
	Chat      Chat   `json:"chat"`
	Text      string `json:"text"`
}

type CallbackQuery struct {
	ID      string   `json:"id"`
	Data    string   `json:"data"`
	Message *Message `json:"message"`
}

type Update struct {
	UpdateID      int64          `json:"update_id"`
	Message       *Message       `json:"message"`
	CallbackQuery *CallbackQuery `json:"callback_query"`
}

type InlineKeyboardButton struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data"`
}

type InlineKeyboardMarkup struct {
	InlineKeyboard [][]InlineKeyboardButton `json:"inline_keyboard"`
}

type BotCommand struct {
	Command     string `json:"command"`
	Description string `json:"description"`
}

type Client struct {
	token   string
	baseURL string
	http    *http.Client
}

func New(token string, pollTimeout time.Duration) *Client {
	return &Client{
		token:   token,
		baseURL: defaultBaseURL,
		http: &http.Client{
			Timeout: pollTimeout + 10*time.Second,
		},
	}
}

func NewWithBaseURL(token, baseURL string, client *http.Client) *Client {
	return &Client{token: token, baseURL: strings.TrimRight(baseURL, "/"), http: client}
}

func (c *Client) GetUpdates(ctx context.Context, offset int64, timeout time.Duration) ([]Update, error) {
	var result []Update
	err := c.call(ctx, "getUpdates", map[string]any{
		"offset":          offset,
		"timeout":         int(timeout.Seconds()),
		"allowed_updates": []string{"message", "callback_query"},
	}, &result)
	return result, err
}

func (c *Client) SendMessage(ctx context.Context, chatID int64, text string, silent bool, keyboard *InlineKeyboardMarkup) error {
	payload := map[string]any{
		"chat_id":              chatID,
		"text":                 text,
		"disable_notification": silent,
	}
	if keyboard != nil {
		payload["reply_markup"] = keyboard
	}
	return c.call(ctx, "sendMessage", payload, nil)
}

func (c *Client) AnswerCallback(ctx context.Context, callbackID, text string) error {
	return c.call(ctx, "answerCallbackQuery", map[string]any{
		"callback_query_id": callbackID,
		"text":              text,
	}, nil)
}

func (c *Client) EditMessageReplyMarkup(ctx context.Context, chatID, messageID int64, keyboard *InlineKeyboardMarkup) error {
	return c.call(ctx, "editMessageReplyMarkup", map[string]any{
		"chat_id":      chatID,
		"message_id":   messageID,
		"reply_markup": keyboard,
	}, nil)
}

func (c *Client) SetCommands(ctx context.Context, commands []BotCommand) error {
	return c.call(ctx, "setMyCommands", map[string]any{"commands": commands}, nil)
}

func (c *Client) call(ctx context.Context, method string, payload any, dst any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode Telegram request: %w", err)
	}
	endpoint := c.baseURL + "/bot" + c.token + "/" + method
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create Telegram request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("call Telegram API: %w", err)
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("read Telegram response: %w", err)
	}

	var envelope struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		Description string          `json:"description"`
		ErrorCode   int             `json:"error_code"`
	}
	if err := json.Unmarshal(responseBody, &envelope); err != nil {
		return fmt.Errorf("decode Telegram response: %w", err)
	}
	if !envelope.OK {
		description := envelope.Description
		if description == "" {
			description = strings.TrimSpace(string(responseBody))
		}
		return fmt.Errorf("Telegram API error %s: %s", strconv.Itoa(envelope.ErrorCode), description)
	}
	if dst != nil && len(envelope.Result) > 0 {
		if err := json.Unmarshal(envelope.Result, dst); err != nil {
			return fmt.Errorf("decode Telegram result: %w", err)
		}
	}
	return nil
}
