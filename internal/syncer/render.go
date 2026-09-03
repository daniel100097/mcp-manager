package syncer

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/pelletier/go-toml/v2"
	"github.com/tidwall/jsonc"

	"github.com/daniel100097/mcp-manager/internal/config"
)

func mergeJSON(existing []byte, key string, servers map[string]any) ([]byte, error) {
	document := map[string]json.RawMessage{}
	if len(bytes.TrimSpace(existing)) > 0 {
		standardized := jsonc.ToJSON(existing)
		if err := config.ValidateJSON(standardized); err != nil {
			return nil, err
		}
		decoder := json.NewDecoder(bytes.NewReader(standardized))
		if err := decoder.Decode(&document); err != nil {
			return nil, err
		}
		if err := jsonEOF(decoder); err != nil {
			return nil, err
		}
	}

	if len(servers) == 0 {
		delete(document, key)
	} else {
		encodedServers, err := json.Marshal(servers)
		if err != nil {
			return nil, err
		}
		document[key] = encodedServers
	}
	output, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(output, '\n'), nil
}

func mergeTOML(existing []byte, key string, servers map[string]any) ([]byte, error) {
	document := map[string]any{}
	if len(bytes.TrimSpace(existing)) > 0 {
		if err := toml.Unmarshal(existing, &document); err != nil {
			return nil, err
		}
	}
	if len(servers) == 0 {
		delete(document, key)
	} else {
		document[key] = servers
	}
	output, err := toml.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("encode TOML: %w", err)
	}
	return output, nil
}

func jsonEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); errors.Is(err, io.EOF) {
		return nil
	} else if err != nil {
		return err
	}
	return errors.New("multiple JSON values are not allowed")
}
