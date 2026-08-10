package app

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
)

type messageListCursor struct {
	ReceivedAt string `json:"receivedAt"`
	ID         string `json:"id"`
}

func encodeMessageListCursor(receivedAt time.Time, id string) string {
	payload, _ := json.Marshal(messageListCursor{ReceivedAt: receivedAt.UTC().Format(time.RFC3339Nano), ID: id})
	return base64.RawURLEncoding.EncodeToString(payload)
}

func parseMessageListCursor(raw string) (receivedAt, id string, offset int, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", 0, nil
	}
	if n, convErr := strconv.Atoi(raw); convErr == nil {
		if n < 0 {
			return "", "", 0, errors.New("invalid cursor")
		}
		return "", "", n, nil
	}
	data, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return "", "", 0, errors.New("invalid cursor")
	}
	var cursor messageListCursor
	if err := json.Unmarshal(data, &cursor); err != nil || cursor.ReceivedAt == "" || cursor.ID == "" {
		return "", "", 0, errors.New("invalid cursor")
	}
	if _, err := time.Parse(time.RFC3339Nano, cursor.ReceivedAt); err != nil {
		return "", "", 0, errors.New("invalid cursor")
	}
	return cursor.ReceivedAt, cursor.ID, 0, nil
}
