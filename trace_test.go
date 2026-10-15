package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestMASTraceCapturesEveryDeliveryTransition(t *testing.T) {
	b := testBus(t)
	var err error
	b.trace, err = openTrace(filepath.Dir(b.database))
	if err != nil {
		t.Fatal(err)
	}
	defer b.trace.close()
	request, err := b.begin("release", "east-lead", "test", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.snapshot("inspection", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := b.consume(request); err != nil {
		t.Fatal(err)
	}
	if _, err := b.finish(request, map[string]any{"state": "ready"}); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(filepath.Join(filepath.Dir(b.database), "mailbox.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var records []traceRecord
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var record traceRecord
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	want := []string{"send", "inspection", "receive", "ack", "send", "receive", "ack", "result-delivered"}
	if len(records) != len(want) {
		t.Fatalf("got %d transitions, want %d", len(records), len(want))
	}
	for i, event := range want {
		if records[i].Event != event || records[i].Time.IsZero() {
			t.Fatalf("bad transition %d: %+v", i, records[i])
		}
	}
	if records[1].Status.Messages[0].Received {
		t.Fatal("status inspection consumed a message")
	}
	if !records[2].Status.Messages[0].Received || records[2].Status.Messages[0].Acked {
		t.Fatal("receive transition missing")
	}
	if records[0].Pending["east-lead"] != 1 || len(records[3].Pending) != 0 {
		t.Fatal("pending mailbox changes were not captured")
	}
	last := records[len(records)-1]
	if len(last.Pending) != 0 || last.Status.Tasks[0].State != "completed" {
		t.Fatalf("bad final snapshot: %+v", last)
	}
}
