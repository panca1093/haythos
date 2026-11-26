package app_test

import (
	"encoding/json"
	"testing"

	"github.com/panca/haythos/core/task"
)

func TestTargetsAndReplyToStayReadableForOldEvents(t *testing.T) {
	var c task.Created
	if err := json.Unmarshal([]byte(`{"job":"chat","reply_to":"dm-1"}`), &c); err != nil || c.ReplyTo != "dm-1" || len(c.Targets) != 0 {
		t.Fatalf("old created event = %+v, %v", c, err)
	}
}
