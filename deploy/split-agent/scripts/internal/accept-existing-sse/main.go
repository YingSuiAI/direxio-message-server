package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

type config struct {
	HTTPBase          string         `json:"http_base"`
	SessionFile       string         `json:"session_file"`
	Params            map[string]any `json:"params"`
	Expect            string         `json:"expect"`
	Reconnect         bool           `json:"reconnect"`
	StopAfterAccepted bool           `json:"stop_after_accepted"`
	OutputFile        string         `json:"output_file"`
	ReceiptFile       string         `json:"receipt_file"`
	AuditFile         string         `json:"audit_file"`
}

type session struct {
	AccessToken string `json:"access_token"`
}

func main() {
	if len(os.Args) != 2 {
		fatal(errors.New("usage: accept-existing-sse CONFIG_JSON"))
	}
	var cfg config
	if err := readJSON(os.Args[1], &cfg); err != nil {
		fatal(err)
	}
	var auth session
	if err := readJSON(cfg.SessionFile, &auth); err != nil {
		fatal(err)
	}
	conversationID, _ := cfg.Params["conversation_id"].(string)
	if strings.TrimSpace(auth.AccessToken) == "" || cfg.HTTPBase == "" || cfg.OutputFile == "" || uuid.Validate(conversationID) != nil || (cfg.Expect != "done" && cfg.Expect != "error") {
		fatal(errors.New("invalid SSE acceptance config"))
	}
	out, err := os.OpenFile(cfg.OutputFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		fatal(err)
	}
	defer out.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	receipt, err := startTurn(ctx, cfg.HTTPBase, auth.AccessToken, conversationID, cfg.Params)
	if err != nil {
		fatal(err)
	}
	if cfg.ReceiptFile != "" {
		if err := writeJSONFile(cfg.ReceiptFile, receipt); err != nil {
			fatal(err)
		}
	}
	if cfg.StopAfterAccepted {
		if err := stopDurableTurn(ctx, cfg.HTTPBase, auth.AccessToken, receipt); err != nil {
			fatal(err)
		}
	}
	after := int64(0)
	terminal := false
	if cfg.Reconnect {
		after, terminal, err = watchTurn(ctx, cfg.HTTPBase, auth.AccessToken, receipt, after, true, cfg.Expect, out)
		if err != nil {
			fatal(err)
		}
	}
	if !terminal {
		after, terminal, err = watchTurn(ctx, cfg.HTTPBase, auth.AccessToken, receipt, after, false, cfg.Expect, out)
		if err != nil {
			fatal(err)
		}
	}
	if after <= 0 || !terminal {
		fatal(errors.New("SSE stream returned no terminal positive sequence"))
	}
	if cfg.AuditFile != "" {
		audit := map[string]any{
			"turn_post_count": 1,
			"watch_get_count": 1,
			"conversation_id": stringValue(receipt["conversation_id"]),
			"turn_id":         stringValue(receipt["turn_id"]),
			"idempotency_key": stringValue(receipt["idempotency_key"]),
			"terminal_seq":    after,
			"terminal":        cfg.Expect,
		}
		if cfg.Reconnect {
			audit["watch_get_count"] = 2
		}
		if err := writeJSONFile(cfg.AuditFile, audit); err != nil {
			fatal(err)
		}
	}
}

func startTurn(ctx context.Context, base, token, conversationID string, params map[string]any) (map[string]any, error) {
	body, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	endpoint := strings.TrimRight(base, "/") + "/_p2p/agent/chat/conversations/" + url.PathEscape(conversationID) + "/turns"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	var receipt map[string]any
	if response.StatusCode != http.StatusAccepted || json.NewDecoder(response.Body).Decode(&receipt) != nil {
		return nil, fmt.Errorf("turn admission returned HTTP %d", response.StatusCode)
	}
	if uuid.Validate(stringValue(receipt["turn_id"])) != nil || integer(receipt["seq"]) <= 0 || integer(receipt["revision"]) <= 0 {
		return nil, errors.New("turn admission returned an invalid receipt")
	}
	return receipt, nil
}

func watchTurn(ctx context.Context, base, token string, receipt map[string]any, after int64, disconnectAfterFirst bool, expect string, out io.Writer) (int64, bool, error) {
	conversationID := stringValue(receipt["conversation_id"])
	turnID := stringValue(receipt["turn_id"])
	endpoint := strings.TrimRight(base, "/") + "/_p2p/agent/chat/conversations/" + url.PathEscape(conversationID) + "/turns/" + url.PathEscape(turnID) + "/events?after_seq=" + strconv.FormatInt(after, 10)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return after, false, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Last-Event-ID", strconv.FormatInt(after, 10))
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		return after, false, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return after, false, fmt.Errorf("turn events returned HTTP %d", response.StatusCode)
	}
	scanner := bufio.NewScanner(response.Body)
	maxSeq := after
	frames := 0
	for {
		frame, ok, err := readSSEFrame(scanner)
		if err != nil {
			return maxSeq, false, err
		}
		if !ok {
			return maxSeq, false, errors.New("SSE stream closed without a terminal event")
		}
		seq := integer(frame["seq"])
		if seq <= after {
			return maxSeq, false, fmt.Errorf("SSE resume replayed sequence %d at cursor %d", seq, after)
		}
		if err := json.NewEncoder(out).Encode(frame); err != nil {
			return maxSeq, false, err
		}
		if seq > maxSeq {
			maxSeq = seq
		}
		frames++
		event := stringValue(frame["event"])
		if event == "done" || event == "error" {
			if event != expect {
				return maxSeq, false, fmt.Errorf("expected terminal %s, got %s", expect, event)
			}
			return maxSeq, true, nil
		}
		if disconnectAfterFirst && frames == 1 {
			return maxSeq, false, nil
		}
	}
}

func readSSEFrame(scanner *bufio.Scanner) (map[string]any, bool, error) {
	var event, data string
	var id int64
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if data == "" {
				continue
			}
			var frame map[string]any
			if err := json.Unmarshal([]byte(data), &frame); err != nil {
				return nil, false, err
			}
			if integer(frame["seq"]) != id || stringValue(frame["event"]) != event {
				return nil, false, errors.New("SSE frame metadata does not match its data envelope")
			}
			return frame, true, nil
		}
		key, value, found := strings.Cut(line, ":")
		if !found || strings.HasPrefix(line, ":") {
			continue
		}
		value = strings.TrimPrefix(value, " ")
		switch key {
		case "id":
			id, _ = strconv.ParseInt(value, 10, 64)
		case "event":
			event = value
		case "data":
			data += value
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, false, err
	}
	return nil, false, nil
}

func stopDurableTurn(ctx context.Context, base, token string, receipt map[string]any) error {
	turnID := stringValue(receipt["turn_id"])
	if uuid.Validate(turnID) != nil {
		return errors.New("turn receipt has invalid stop identity")
	}
	return postAction(ctx, base, token, "agent.chat.turn.stop", map[string]any{
		"idempotency_key": uuid.NewString(), "turn_id": turnID,
	})
}

func postAction(ctx context.Context, base, token, action string, params map[string]any) error {
	body, err := json.Marshal(map[string]any{"action": action, "params": params})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/_p2p/command", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("%s returned HTTP %d", action, response.StatusCode)
	}
	_, err = io.Copy(io.Discard, response.Body)
	return err
}

func readJSON(path string, target any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, target)
}

func writeJSONFile(path string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o600)
}

func stringValue(value any) string {
	text, _ := value.(string)
	return strings.TrimSpace(text)
}

func integer(value any) int64 {
	switch value := value.(type) {
	case float64:
		return int64(value)
	case json.Number:
		parsed, _ := value.Int64()
		return parsed
	default:
		return 0
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "SSE acceptance:", err)
	os.Exit(1)
}
