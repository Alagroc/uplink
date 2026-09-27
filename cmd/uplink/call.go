package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

// runCall invokes one operator tool straight from the shell.
//
// This exists so the whole system can be driven, scripted and tested without an
// LLM in the loop — which is also how uplink's own end-to-end tests work.
func runCall(ctx context.Context, args []string) error {
	fs := flagSet("call", "invoke one operator tool from the shell")
	groundFlag := fs.String("ground", "", "ground base URL (default $UPLINK_GROUND or http://127.0.0.1:8765)")
	tokenFile := fs.String("token-file", "", "token file (default ~/.uplink/token)")
	list := fs.Bool("tools", false, "list the available tools and their arguments")
	raw := fs.Bool("raw", false, "print the raw JSON-RPC response")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `uplink call — invoke one operator tool from the shell

Usage:
  uplink call --tools
  uplink call <tool> ['<json args>']

Examples:
  uplink call list_crew
  uplink call submit_job '{"kind":"exec","crew":"devbox","command":"uname -a"}'
  uplink call job_logs '{"job_id":"job_abc123"}'
  uplink call inbox
  uplink call reply '{"question_id":"q_1","answer":"use the local-path storage class"}'

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	token, err := loadToken(*tokenFile, false)
	if err != nil {
		return err
	}
	endpoint := groundURL(*groundFlag) + "/mcp"

	var method string
	params := map[string]any{}
	switch {
	case *list:
		method = "tools/list"
	case fs.NArg() == 0:
		fs.Usage()
		return fmt.Errorf("give a tool name, or --tools to list them")
	default:
		method = "tools/call"
		argsJSON := map[string]any{}
		if fs.NArg() > 1 {
			if err := json.Unmarshal([]byte(fs.Arg(1)), &argsJSON); err != nil {
				return fmt.Errorf("arguments must be a JSON object: %w", err)
			}
		}
		params = map[string]any{"name": fs.Arg(0), "arguments": argsJSON}
	}

	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": method, "params": params,
	})
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach ground at %s: %w", endpoint, err)
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ground returned %d: %s", resp.StatusCode, strings.TrimSpace(string(payload)))
	}
	if *raw {
		fmt.Println(strings.TrimSpace(string(payload)))
		return nil
	}

	return printToolResponse(payload)
}

func printToolResponse(payload []byte) error {
	var envelope struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
			Tools   []struct {
				Name        string `json:"name"`
				Description string `json:"description"`
			} `json:"tools"`
		} `json:"result"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		fmt.Println(strings.TrimSpace(string(payload)))
		return nil
	}
	if envelope.Error != nil {
		return fmt.Errorf("%s (code %d)", envelope.Error.Message, envelope.Error.Code)
	}
	if tools := envelope.Result.Tools; len(tools) > 0 {
		for _, t := range tools {
			fmt.Printf("%-14s %s\n", t.Name, t.Description)
		}
		return nil
	}
	for _, c := range envelope.Result.Content {
		fmt.Println(c.Text)
	}
	if envelope.Result.IsError {
		return fmt.Errorf("tool reported an error")
	}
	return nil
}
