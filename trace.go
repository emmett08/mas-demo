package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type protocolStatus struct {
	Messages []struct {
		ID        string `json:"id"`
		Sender    string `json:"sender"`
		Recipient string `json:"recipient"`
		Received  bool   `json:"received"`
		Acked     bool   `json:"acked"`
	} `json:"messages"`
	Tasks []struct {
		ID    string `json:"task_id"`
		State string `json:"state"`
	} `json:"tasks"`
}

type traceRecord struct {
	Time      time.Time      `json:"time"`
	Event     string         `json:"event"`
	Actor     string         `json:"actor,omitempty"`
	MessageID string         `json:"message_id,omitempty"`
	Pending   map[string]int `json:"pending"`
	Status    protocolStatus `json:"status"`
}

type protocolTrace struct {
	history *os.File
	summary *os.File
}

func openTrace(dir string) (*protocolTrace, error) {
	history, err := os.OpenFile(filepath.Join(dir, "mailbox.jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	summary, err := os.OpenFile(filepath.Join(dir, "mailbox.md"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		history.Close()
		return nil, err
	}
	t := &protocolTrace{history: history, summary: summary}
	_, err = fmt.Fprintln(summary, "### MAS mailbox timeline\n\nSnapshots use status, never receive. Task completion is not rollout approval.\n\n| UTC | Operation | Actor | Message / route | Pending | Open tasks | Completed tasks |\n| --- | --- | --- | --- | --- | --- | --- |")
	if err != nil {
		t.close()
		return nil, err
	}
	return t, nil
}

func (t *protocolTrace) close() {
	t.history.Close()
	t.summary.Close()
}

func (b *bus) snapshot(event, actor, messageID string) error {
	if b.trace == nil {
		return nil
	}
	var status protocolStatus
	if err := b.command(nil, &status, "status", b.database, "rollout"); err != nil {
		return err
	}
	record := traceRecord{
		Time: time.Now().UTC(), Event: event, Actor: actor, MessageID: messageID,
		Pending: make(map[string]int), Status: status,
	}
	pending, open, completed := 0, 0, 0
	route := messageID
	for _, m := range status.Messages {
		if !m.Acked {
			record.Pending[m.Recipient]++
			pending++
		}
		if m.ID == messageID || (event == "send" && m.ID == status.Messages[len(status.Messages)-1].ID) {
			route = m.ID + ": " + m.Sender + " → " + m.Recipient
		}
	}
	for _, task := range status.Tasks {
		if task.State == "open" {
			open++
		}
		if task.State == "completed" {
			completed++
		}
	}
	if err := json.NewEncoder(b.trace.history).Encode(record); err != nil {
		return err
	}
	_, err := fmt.Fprintf(b.trace.summary, "| %s | %s | %s | %s | %d | %d | %d |\n",
		record.Time.Format("15:04:05.000"), event, actor, route, pending, open, completed)
	return err
}
