// Package registryjson contains strict JSON primitives for built-in registries.
package registryjson

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
)

const maxDepth, maxNodes = 32, 4096

func CanonicalValue(content []byte) (interface{}, error) {
	dec := json.NewDecoder(bytes.NewReader(content))
	dec.UseNumber()
	value, err := readCanonicalValue(dec)
	if err != nil {
		return nil, err
	}
	if err := checkLimits(value, 0, new(int)); err != nil {
		return nil, err
	}
	var trailing interface{}
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, errors.New("trailing JSON")
		}
		return nil, err
	}
	return value, nil
}

func ContentDigest(content []byte) string {
	value, err := CanonicalValue(content)
	if err != nil {
		return ""
	}
	raw, ok := value.(map[string]interface{})
	if !ok {
		return ""
	}
	delete(raw, "content_digest")
	canonical, err := json.Marshal(raw)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}

func SortedUnique(values []string) bool {
	if !sort.StringsAreSorted(values) {
		return false
	}
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		seen[value] = struct{}{}
	}
	return len(seen) == len(values)
}

func checkLimits(value interface{}, depth int, nodes *int) error {
	if depth > maxDepth {
		return errors.New("JSON depth cap exceeded")
	}
	(*nodes)++
	if *nodes > maxNodes {
		return errors.New("JSON node cap exceeded")
	}
	switch value := value.(type) {
	case map[string]interface{}:
		for _, child := range value {
			if err := checkLimits(child, depth+1, nodes); err != nil {
				return err
			}
		}
	case []interface{}:
		for _, child := range value {
			if err := checkLimits(child, depth+1, nodes); err != nil {
				return err
			}
		}
	}
	return nil
}

func readCanonicalValue(dec *json.Decoder) (interface{}, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch delim := tok.(type) {
	case json.Delim:
		switch delim {
		case '{':
			value := map[string]interface{}{}
			for dec.More() {
				keyToken, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key := keyToken.(string)
				if _, exists := value[key]; exists {
					return nil, fmt.Errorf("duplicate key %q", key)
				}
				value[key], err = readCanonicalValue(dec)
				if err != nil {
					return nil, err
				}
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return value, nil
		case '[':
			value := []interface{}{}
			for dec.More() {
				child, err := readCanonicalValue(dec)
				if err != nil {
					return nil, err
				}
				value = append(value, child)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return value, nil
		}
	}
	return tok, nil
}
