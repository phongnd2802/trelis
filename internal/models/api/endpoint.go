package api

import "net/http"

type AuthFunc func(req *http.Request, body []byte)

type Endpoint struct {
	BaseURL string
	URL     string
	Model   string
	ModelID string
	Auth    AuthFunc
	Headers map[string]string
	Client  *http.Client
}
