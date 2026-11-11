package maintenancehost

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"time"
)

type Client struct{ socket string }

func NewClient(socket string) *Client { return &Client{socket: socket} }

func (c *Client) call(ctx context.Context, method, path string, input, output any) error {
	if !absolute(c.socket) {
		return ErrUnavailable
	}
	var body bytes.Buffer
	if input != nil && json.NewEncoder(&body).Encode(input) != nil {
		return ErrUnavailable
	}
	transport := &http.Transport{DisableKeepAlives: true, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", c.socket)
		if err != nil {
			return nil, ErrUnavailable
		}
		uid, err := peerUID(conn)
		if err != nil || uid != 0 {
			conn.Close()
			return nil, ErrUnavailable
		}
		return conn, nil
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return ErrUnavailable }}
	req, err := http.NewRequestWithContext(ctx, method, "http://maintenance.local/v1"+path, &body)
	if err != nil {
		return ErrUnavailable
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := client.Do(req)
	if err != nil {
		return ErrUnavailable
	}
	defer res.Body.Close()
	switch res.StatusCode {
	case http.StatusNotFound:
		return ErrNotFound
	case http.StatusConflict:
		return ErrConflict
	case http.StatusOK, http.StatusAccepted:
	default:
		return ErrUnavailable
	}
	if decode(res.Body, output, 64<<10) != nil {
		return ErrUnavailable
	}
	return nil
}

func (c *Client) Plans(ctx context.Context) ([]Plan, error) {
	var v struct {
		Items []Plan `json:"items"`
	}
	e := c.call(ctx, "GET", "/plans", nil, &v)
	return v.Items, e
}
func (c *Client) Submit(ctx context.Context, r Request) (Status, error) {
	var v Status
	if !ValidRequest(r) {
		return v, ErrUnavailable
	}
	e := c.call(ctx, "POST", "/requests", r, &v)
	return v, e
}
func (c *Client) Status(ctx context.Context, id string) (Status, error) {
	var v Status
	if !hexID.MatchString(id) {
		return v, ErrUnavailable
	}
	e := c.call(ctx, "GET", "/requests/"+id, nil, &v)
	return v, e
}
