// SPDX-License-Identifier: AGPL-3.0-or-later

package mcpadapter

import (
	"encoding/json"
	"fmt"

	"github.com/rcarback/taskd/internal/client"
	"github.com/rcarback/taskd/internal/proto"
)

// call sends one verb to the daemon and decodes its result.
//
// It starts a daemon if none is listening, because client.Call does. A
// daemon-reported failure becomes an ordinary error, which AddTool turns
// into a tool result with IsError set, so the agent reads the message
// rather than a transport failure.
func call[In, Out any](root string, verb proto.Verb, in In) (Out, error) {
	var out Out

	params, err := json.Marshal(in)
	if err != nil {
		return out, fmt.Errorf("mcp: encode %s params: %w", verb, err)
	}

	res, err := client.Call(root, proto.Request{Verb: verb, Params: params})
	if err != nil {
		return out, fmt.Errorf("mcp: %s: %w", verb, err)
	}
	if !res.OK {
		return out, fmt.Errorf("mcp: %s: %s", verb, res.Error)
	}
	if err := json.Unmarshal(res.Result, &out); err != nil {
		return out, fmt.Errorf("mcp: decode %s result: %w", verb, err)
	}
	return out, nil
}
