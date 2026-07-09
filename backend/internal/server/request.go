package server

import (
	"encoding/json"
	"io"
	"net/http"
	"unicode/utf8"
)

var errInvalidUTF8 = jsonInvalidUTF8Error{}

type jsonInvalidUTF8Error struct{}

func (jsonInvalidUTF8Error) Error() string { return "request body must be valid UTF-8" }

// DecodeBody reads and decodes a JSON request body into a value of type T.
// It rejects request bodies that are not valid UTF-8.
func DecodeBody[T any](r *http.Request) (T, error) {
	var v T
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return v, err
	}
	if !utf8.Valid(raw) {
		return v, errInvalidUTF8
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return v, err
	}
	return v, nil
}
