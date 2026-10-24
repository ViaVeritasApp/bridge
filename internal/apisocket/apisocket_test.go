package apisocket

import "testing"

func TestParseEvent(t *testing.T) {
	e, ok := parseEvent(`42/bridges,17["command",{"id":"x","type":"delete","paths":["/a.pdf"]}]`)
	if !ok || e.name != "command" || e.ackID != "17" || string(e.data) != `{"id":"x","type":"delete","paths":["/a.pdf"]}` {
		t.Fatalf("parsed %+v ok=%v", e, ok)
	}

	e, ok = parseEvent(`42/bridges,["exception",{"status":"error"}]`)
	if !ok || e.name != "exception" || e.ackID != "" {
		t.Fatalf("no-ack event parsed %+v ok=%v", e, ok)
	}

	for _, bad := range []string{`42["command",{}]`, `42/other,1["command",{}]`, `42/bridges,1[]`, `42/bridges,1not json`} {
		if _, ok := parseEvent(bad); ok {
			t.Errorf("%q accepted", bad)
		}
	}
}
