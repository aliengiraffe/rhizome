package oai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRespondContract(t *testing.T) {
	f := newFixture(t)
	f.signin()
	for _, contentType := range []string{"text/event-stream", "", "application/json"} {
		t.Run("content-type-"+contentType, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/responses" || r.Header.Get("Authorization") != "Bearer synthetic-access" {
					t.Error("invalid request")
				}
				var body struct {
					Request
					Store  *bool `json:"store"`
					Stream bool  `json:"stream"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if body.Store == nil || *body.Store || !body.Stream || body.Instructions != "instructions" || len(body.Input) != 2 || body.Input[1].Content != "second" {
					t.Error("request contract")
				}
				if contentType != "" {
					w.Header().Set("Content-Type", contentType)
				} else {
					w.Header()["Content-Type"] = nil
				}
				io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"answer\"}\n\ndata: {\"type\":\"response.completed\"}\n\n")
			}))
			defer server.Close()
			f.c.api = server.URL
			result, err := f.c.Respond(context.Background(), Request{Model: "explicit", Instructions: "instructions", Input: []Message{{Role: "user", Content: "first"}, {Role: "assistant", Content: "second"}}})
			if contentType == "application/json" {
				if err == nil {
					t.Fatal("accepted wrong stream type")
				}
			} else if err != nil || result != "answer" {
				t.Fatal(result, err)
			}
		})
	}
	for _, req := range []Request{{}, {Input: []Message{{Role: "system", Content: "x"}}}, {Instructions: strings.Repeat("x", 2<<20), Input: []Message{{Role: "user", Content: "x"}}}} {
		if _, err := f.c.Respond(context.Background(), req); err == nil {
			t.Fatal("accepted invalid input")
		}
	}
}
