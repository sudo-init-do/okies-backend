package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

type FlutterwaveClient struct {
	SecretKey string
	BaseURL   string
	Client    *http.Client
}

func NewFlutterwaveClient() *FlutterwaveClient {
	return &FlutterwaveClient{
		SecretKey: os.Getenv("FLW_SECRET_KEY"),
		BaseURL:   getenv("FLW_BASE_URL", "https://api.flutterwave.com"),
		Client:    &http.Client{Timeout: 10 * time.Second},
	}
}

func (f *FlutterwaveClient) do(ctx context.Context, method, path string, body any) (map[string]any, error) {
	url := fmt.Sprintf("%s%s", f.BaseURL, path)

	var buf io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		buf = bytes.NewBuffer(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, buf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+f.SecretKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := f.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	if resp.StatusCode >= 400 {
		return result, fmt.Errorf("flutterwave error: %v", result)
	}

	return result, nil
}
