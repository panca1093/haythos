package readfile

import (
	"context"
	"encoding/json"

	"github.com/panca/haythos/core/task"
)

// TODO: plan step 3 replaces this stub with rg.
var searchSpec = task.ToolSpec{
	Name:        "file.search",
	Description: "Not available yet.",
	InputSchema: json.RawMessage(pathArg + `,"pattern":{"type":"string"}},"required":["pattern"]}`),
}

func (t Guard) search(_ context.Context, _ []byte) (task.ToolResult, error) {
	return refuse("file.search is not available yet")
}
