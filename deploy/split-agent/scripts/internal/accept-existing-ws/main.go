package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/google/uuid"
)

type config struct {
	HTTPBase           string         `json:"http_base"`
	SessionFile        string         `json:"session_file"`
	Params             map[string]any `json:"params"`
	Expect             string         `json:"expect"`
	Reconnect          bool           `json:"reconnect"`
	StopAfterAccepted  bool           `json:"stop_after_accepted"`
	StopAfterReconnect bool           `json:"stop_after_reconnect"`
	OutputFile         string         `json:"output_file"`
}

type session struct {
	AccessToken string `json:"access_token"`
}

func main() {
	if len(os.Args) != 2 {
		fatal(errors.New("usage: accept-existing-ws CONFIG_JSON"))
	}
	var cfg config
	if err := readJSON(os.Args[1], &cfg); err != nil {
		fatal(err)
	}
	var auth session
	if err := readJSON(cfg.SessionFile, &auth); err != nil {
		fatal(err)
	}
	if strings.TrimSpace(auth.AccessToken) == "" || cfg.HTTPBase == "" || cfg.OutputFile == "" || (cfg.Expect != "done" && cfg.Expect != "error") {
		fatal(errors.New("invalid websocket acceptance config"))
	}
	out, err := os.OpenFile(cfg.OutputFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		fatal(err)
	}
	defer out.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	after := int64(0)
	terminal, accepted, err := runConnection(ctx, cfg, auth.AccessToken, after, cfg.Reconnect, nil, out)
	if err != nil {
		fatal(err)
	}
	if cfg.Reconnect {
		after = terminal
		var stopAfterNew map[string]any
		if cfg.StopAfterReconnect {
			if accepted == nil {
				fatal(errors.New("reconnect stream returned no accepted turn identity"))
			}
			stopAfterNew = accepted
		}
		terminal, _, err = runConnection(ctx, cfg, auth.AccessToken, after, false, stopAfterNew, out)
		if err != nil {
			fatal(err)
		}
	}
	if terminal <= 0 {
		fatal(errors.New("stream returned no positive sequence"))
	}
}

func runConnection(ctx context.Context, cfg config, token string, after int64, disconnectAfterFirst bool, stopAfterNew map[string]any, out io.Writer) (int64, map[string]any, error) {
	ticket, err := issueTicket(ctx, cfg.HTTPBase, token)
	if err != nil {
		return 0, nil, err
	}
	parsed, err := url.Parse(cfg.HTTPBase)
	if err != nil {
		return 0, nil, err
	}
	if parsed.Scheme == "http" {
		parsed.Scheme = "ws"
	} else {
		parsed.Scheme = "wss"
	}
	parsed.Path = "/_p2p/ws"
	parsed.RawQuery = url.Values{"ticket": []string{ticket}}.Encode()
	conn, _, err := websocket.Dial(ctx, parsed.String(), nil)
	if err != nil {
		return 0, nil, err
	}
	defer conn.CloseNow()
	if err = wsjson.Write(ctx, conn, map[string]any{"type": "client.hello", "since": 0}); err != nil {
		return 0, nil, err
	}
	var ready map[string]any
	if err = wsjson.Read(ctx, conn, &ready); err != nil || ready["type"] != "server.ready" {
		return 0, nil, fmt.Errorf("server.ready: %w", err)
	}
	params := clone(cfg.Params)
	params["after_seq"] = after
	streamID := fmt.Sprintf("batch-%d", time.Now().UnixNano())
	if err = wsjson.Write(ctx, conn, map[string]any{"type": "client.native_agent_stream", "id": streamID, "action": "agent.chat", "params": params}); err != nil {
		return 0, nil, err
	}
	maxSeq := after
	stopRequested := false
	var accepted map[string]any
	for {
		var frame map[string]any
		if err = wsjson.Read(ctx, conn, &frame); err != nil {
			return maxSeq, accepted, err
		}
		if frame["id"] != streamID {
			continue
		}
		if err = json.NewEncoder(out).Encode(frame); err != nil {
			return maxSeq, accepted, err
		}
		seq := integer(frame["seq"])
		typeName, _ := frame["type"].(string)
		event, _ := frame["event"].(string)
		if typeName == "server.native_agent_stream.accepted" {
			accepted = clone(frame)
		}
		if cfg.StopAfterAccepted && !stopRequested && typeName == "server.native_agent_stream.accepted" {
			if err = stopDurableTurn(ctx, cfg.HTTPBase, token, frame); err != nil {
				return maxSeq, accepted, err
			}
			stopRequested = true
		}
		if stopAfterNew != nil && !stopRequested && seq > after {
			if err = stopDurableTurn(ctx, cfg.HTTPBase, token, stopAfterNew); err != nil {
				return maxSeq, accepted, err
			}
			return seq, accepted, nil
		}
		if typeName == "server.native_agent_stream.error" || event == "error" {
			if cfg.Expect != "error" {
				return maxSeq, accepted, fmt.Errorf("unexpected stream error: %v", frame["error"])
			}
			if seq <= after {
				return maxSeq, accepted, fmt.Errorf("stream error without a new durable sequence: %v", frame["error"])
			}
			return seq, accepted, nil
		}
		if seq <= after {
			return maxSeq, accepted, fmt.Errorf("reconnect replayed sequence %d at cursor %d", seq, after)
		}
		if seq > maxSeq {
			maxSeq = seq
		}
		if disconnectAfterFirst && maxSeq > after && typeName != "server.native_agent_stream.accepted" {
			_ = conn.Close(websocket.StatusGoingAway, "acceptance reconnect")
			return maxSeq, accepted, nil
		}
		if event == "done" {
			if cfg.Expect != "done" {
				return maxSeq, accepted, errors.New("expected stream error, got done")
			}
			return maxSeq, accepted, nil
		}
	}
}

func stopDurableTurn(ctx context.Context, base, token string, accepted map[string]any) error {
	turnID, _ := accepted["turn_id"].(string)
	conversationID, _ := accepted["conversation_id"].(string)
	revision := integer(accepted["revision"])
	if uuid.Validate(turnID) != nil || uuid.Validate(conversationID) != nil || revision <= 0 {
		return errors.New("accepted stream frame has invalid turn identity")
	}
	return postAction(ctx, base, token, "agent.chat.turn.stop", map[string]any{
		"idempotency_key": uuid.NewString(), "turn_id": turnID, "expected_revision": revision,
	}, nil)
}

func postAction(ctx context.Context, base, token, action string, params map[string]any, target any) error {
	body, err := json.Marshal(map[string]any{"action": action, "params": params})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/_p2p/query", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s returned HTTP %d", action, resp.StatusCode)
	}
	if target == nil {
		_, err = io.Copy(io.Discard, resp.Body)
		return err
	}
	return json.NewDecoder(resp.Body).Decode(target)
}

func issueTicket(ctx context.Context, base, token string) (string, error) {
	body := []byte(`{"action":"realtime.ws_ticket.create","params":{}}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/_p2p/query", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var result struct {
		Ticket string `json:"ticket"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&result) != nil || result.Ticket == "" {
		return "", fmt.Errorf("ticket request returned HTTP %d", resp.StatusCode)
	}
	return result.Ticket, nil
}

func readJSON(path string, target any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, target)
}

func clone(in map[string]any) map[string]any {
	out := make(map[string]any, len(in)+1)
	for key, value := range in {
		out[key] = value
	}
	return out
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
	fmt.Fprintln(os.Stderr, "websocket acceptance:", err)
	os.Exit(1)
}
