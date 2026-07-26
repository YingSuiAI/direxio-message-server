package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
)

type options struct {
	baseURL       string
	ownerFile     string
	deepseekFile  string
	extensionFile string
	workloadFile  string
	timeout       time.Duration
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		// Error messages are intentionally stable and contain no response body,
		// token, secret, or provider text.
		fmt.Fprintln(os.Stderr, "agent-core-e2e:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: full|deepseek|extensions|workload")
	}
	flow := args[0]
	if flow != "full" && flow != "deepseek" && flow != "extensions" && flow != "workload" {
		return errors.New("unknown flow")
	}
	fs := flag.NewFlagSet("agent-core-e2e", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	op := options{}
	fs.StringVar(&op.baseURL, "base-url", os.Getenv("DIREXTALK_E2E_BASE_URL"), "Message Server base URL")
	fs.StringVar(&op.ownerFile, "owner-secret-file", os.Getenv("DIREXTALK_E2E_OWNER_SECRET_FILE"), "owner secret file")
	fs.StringVar(&op.deepseekFile, "deepseek-api-key-file", os.Getenv("DIREXTALK_E2E_DEEPSEEK_API_KEY_FILE"), "DeepSeek API key file")
	fs.StringVar(&op.extensionFile, "extension-secret-input-file", os.Getenv("DIREXTALK_E2E_EXTENSION_SECRET_INPUT_FILE"), "extension secret input JSON file")
	fs.StringVar(&op.workloadFile, "workload-plan-file", os.Getenv("DIREXTALK_E2E_WORKLOAD_PLAN_FILE"), "non-secret workload plan JSON file")
	fs.DurationVar(&op.timeout, "timeout", 20*time.Minute, "per-request/flow timeout")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if strings.TrimSpace(op.baseURL) == "" || strings.TrimSpace(op.ownerFile) == "" {
		return errors.New("base-url and owner-secret-file are required")
	}
	owner, err := readSecretFile(op.ownerFile)
	if err != nil {
		return fmt.Errorf("owner secret: %w", err)
	}
	driver, err := NewDriver(op.baseURL, op.timeout)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), op.timeout)
	defer cancel()
	if err := driver.authenticate(ctx, owner); err != nil {
		return err
	}

	summary := Summary{Flow: flow}
	if flow == "deepseek" || flow == "full" {
		if strings.TrimSpace(op.deepseekFile) == "" {
			return errors.New("deepseek-api-key-file is required")
		}
		key, readErr := readSecretFile(op.deepseekFile)
		if readErr != nil {
			return fmt.Errorf("DeepSeek API key: %w", readErr)
		}
		answer, runErr := driver.deepseek(ctx, key)
		if runErr != nil {
			return runErr
		}
		digest := sha256.Sum256([]byte(answer))
		summary.AnswerHash = hex.EncodeToString(digest[:])
		summary.AnswerLen = len([]rune(answer))
		summary.CoreReady = true
	} else {
		if _, err := requireCore(ctx, driver, "agent.info"); err != nil {
			return err
		}
		summary.CoreReady = true
	}
	if flow == "extensions" || flow == "full" {
		count, err := driver.extensions(ctx, op.extensionFile)
		if err != nil {
			return err
		}
		summary.Extensions = count
	}
	if flow == "workload" || flow == "full" {
		if strings.TrimSpace(op.workloadFile) == "" {
			return errors.New("workload-plan-file is required")
		}
		if err := driver.workload(ctx, op.workloadFile); err != nil {
			return err
		}
		summary.Workload = true
	}
	encoded, err := json.Marshal(summary)
	if err != nil {
		return errors.New("cannot encode sanitized summary")
	}
	fmt.Println(string(encoded))
	return nil
}
