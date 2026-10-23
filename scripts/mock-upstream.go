package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"time"
)

var (
	port        = flag.Int("port", 9999, "mock server port")
	minDelay    = flag.Int("min-delay", 0, "minimum response delay (ms)")
	maxDelay    = flag.Int("max-delay", 0, "maximum response delay (ms)")
	errorRate   = flag.Float64("error-rate", 0, "error rate (0-1)")
	slowRate    = flag.Float64("slow-rate", 0, "slow response rate (0-1)")
	slowDelayMs = flag.Int("slow-delay", 5000, "slow response delay (ms)")
)

type ChatResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   Usage    `json:"usage"`
}

type Choice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"`
}

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

func main() {
	flag.Parse()

	http.HandleFunc("/v1/chat/completions", handleChat)
	http.HandleFunc("/health", handleHealth)

	addr := fmt.Sprintf(":%d", *port)
	log.Printf("Mock upstream server starting on %s", addr)
	log.Printf("Config: minDelay=%dms maxDelay=%dms errorRate=%.2f slowRate=%.2f slowDelay=%dms",
		*minDelay, *maxDelay, *errorRate, *slowRate, *slowDelayMs)

	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatal(err)
	}
}

func handleChat(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	// 模拟随机延迟
	if *maxDelay > 0 {
		delay := *minDelay
		if *maxDelay > *minDelay {
			delay += rand.Intn(*maxDelay - *minDelay)
		}
		time.Sleep(time.Duration(delay) * time.Millisecond)
	}

	// 模拟慢请求
	if rand.Float64() < *slowRate {
		log.Printf("[SLOW] Simulating slow response (%dms)", *slowDelayMs)
		time.Sleep(time.Duration(*slowDelayMs) * time.Millisecond)
	}

	// 模拟错误
	if rand.Float64() < *errorRate {
		log.Printf("[ERROR] Simulating error response")
		http.Error(w, `{"error":{"message":"Mock upstream error","type":"server_error"}}`, http.StatusInternalServerError)
		return
	}

	// 正常响应
	resp := ChatResponse{
		ID:      fmt.Sprintf("chatcmpl-mock-%d", time.Now().Unix()),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   "gpt-4o-mini",
		Choices: []Choice{
			{
				Index: 0,
				Message: Message{
					Role:    "assistant",
					Content: "Hello from mock upstream!",
				},
				FinishReason: "stop",
			},
		},
		Usage: Usage{
			PromptTokens:     10,
			CompletionTokens: 5,
			TotalTokens:      15,
		},
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)

	log.Printf("[OK] %s %s - %dms", r.Method, r.URL.Path, time.Since(start).Milliseconds())
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}
