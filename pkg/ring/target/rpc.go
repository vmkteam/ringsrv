package target

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// RPCGuarded reports whether the profile allowlists JSON-RPC methods: the
// caller has to know whether to look at the body at all.
func (p *Profile) RPCGuarded() bool { return len(p.rpcMethods) > 0 }

// rpcRequest is the part of a JSON-RPC request the allowlist reads. Nothing
// else is decoded: params are the upstream's business, and the id, if any,
// is the caller's.
type rpcRequest struct {
	Method *string `json:"method"`
}

// CheckRPC refuses a body whose methods are not all in AllowRPCMethods, or
// which is not a JSON-RPC request at all. On a profile without the
// allowlist it is a no-op, so the call site does not have to ask first.
//
// The body has to be one object or a non-empty array of objects, each with a
// string method. Anything else — an empty body, a number, an object without
// a method, a batch with a hole in it — is refused rather than passed on:
// what the allowlist cannot read, it cannot allow. The error says which
// method it stopped at, because on a one-path API the method is what the
// caller has to fix.
func (p *Profile) CheckRPC(body string) error {
	if !p.RPCGuarded() {
		return nil
	}
	raw := strings.TrimSpace(body)
	if raw == "" {
		return errors.New("body is empty — the target takes JSON-RPC requests")
	}

	var reqs []rpcRequest
	switch raw[0] {
	case '{':
		var one rpcRequest
		if err := json.Unmarshal([]byte(raw), &one); err != nil {
			return fmt.Errorf("body is not a JSON-RPC request: %w", err)
		}
		reqs = []rpcRequest{one}
	case '[':
		if err := json.Unmarshal([]byte(raw), &reqs); err != nil {
			return fmt.Errorf("body is not a JSON-RPC batch: %w", err)
		}
		if len(reqs) == 0 {
			return errors.New("body is an empty batch")
		}
	default:
		return errors.New("body is not a JSON-RPC request: want an object or a batch array")
	}

	for i, r := range reqs {
		if r.Method == nil || *r.Method == "" {
			if len(reqs) > 1 {
				return fmt.Errorf("batch element %d has no method", i)
			}
			return errors.New("request has no method")
		}
		if !p.rpcMethods[strings.ToLower(*r.Method)] {
			return fmt.Errorf("method %q is not allowed", *r.Method)
		}
	}
	return nil
}
