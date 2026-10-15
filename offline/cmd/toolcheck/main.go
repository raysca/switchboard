// Command toolcheck reports whether an Ollama model completes a tool-call round trip on /v1/responses
// the way the xAI adapter uses it. Usage: toolcheck -model llama3.2:3b [-url http://127.0.0.1:11434]
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

var tools = []map[string]any{{
	"type": "function", "name": "transfer_to_human", "description": "Transfer the caller to a human agent",
	"parameters": map[string]any{"type": "object", "properties": map[string]any{"reason": map[string]any{"type": "string"}}, "required": []string{"reason"}},
}}

type event struct {
	Type  string `json:"type"`
	Delta string `json:"delta"`
	Resp  struct {
		Output []json.RawMessage `json:"output"`
	} `json:"response"`
}

func call(url, model string, input []any) (text string, outputs []json.RawMessage, err error) {
	body, _ := json.Marshal(map[string]any{"model": model, "store": false, "stream": true, "tools": tools, "input": input})
	resp, err := (&http.Client{Timeout: 3 * time.Minute}).Post(url+"/v1/responses", "application/json", bytes.NewReader(body))
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 8<<20)
	for sc.Scan() {
		line, ok := strings.CutPrefix(sc.Text(), "data:")
		if !ok {
			continue
		}
		var e event
		if json.Unmarshal([]byte(line), &e) != nil {
			continue
		}
		switch e.Type {
		case "response.output_text.delta":
			text += e.Delta
		case "response.completed":
			outputs = e.Resp.Output
		}
	}
	return text, outputs, sc.Err()
}

func main() {
	model := flag.String("model", "", "Ollama model name")
	url := flag.String("url", "http://127.0.0.1:11434", "Ollama base URL")
	flag.Parse()
	if *model == "" {
		fmt.Fprintln(os.Stderr, "-model is required")
		os.Exit(2)
	}
	start := time.Now()
	sys := map[string]any{"role": "system", "content": "You are a dental receptionist. Use tools when appropriate. Be brief."}
	user := map[string]any{"role": "user", "content": "Please transfer me to a human, I'm upset about my bill."}
	_, outs, err := call(*url, *model, []any{sys, user})
	if err != nil {
		fmt.Println("FAIL round 1:", err)
		os.Exit(1)
	}
	var callID string
	input := []any{sys, user}
	for _, o := range outs {
		var item map[string]any
		_ = json.Unmarshal(o, &item)
		input = append(input, item)
		if item["type"] == "function_call" {
			callID, _ = item["call_id"].(string)
		}
	}
	if callID == "" {
		fmt.Println("FAIL: the model did not call the tool")
		os.Exit(1)
	}
	input = append(input, map[string]any{"type": "function_call_output", "call_id": callID, "output": `{"status":"transferred"}`})
	text, _, err := call(*url, *model, input)
	if err != nil || strings.TrimSpace(text) == "" {
		fmt.Println("FAIL round 2:", err, "text:", text)
		os.Exit(1)
	}
	fmt.Printf("OK  model=%s tool-call+result round trip in %s\n    reply: %s\n", *model, time.Since(start).Round(time.Millisecond), strings.TrimSpace(text))
}
