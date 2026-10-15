package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"reflect"
	"strings"
)

// bus is a trusted host adapter, not an authentication boundary.
type bus struct {
	ctx      context.Context
	binary   string
	database string
	next     int
	advisor  *advisor
	trace    *protocolTrace
}

type message struct {
	ID        string         `json:"message_id"`
	Run       string         `json:"run_id"`
	Sender    string         `json:"sender"`
	Recipient string         `json:"recipient"`
	Version   string         `json:"version"`
	Kind      string         `json:"kind"`
	Payload   map[string]any `json:"payload"`
}

func (b *bus) command(input any, output any, args ...string) error {
	var stdin bytes.Buffer
	if input != nil {
		if err := json.NewEncoder(&stdin).Encode(input); err != nil {
			return err
		}
	}
	return b.execute(&stdin, output, args...)
}

func (b *bus) execute(stdin io.Reader, output any, args ...string) error {
	cmd := exec.CommandContext(b.ctx, b.binary, args...)
	cmd.Stdin = stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if b.ctx.Err() != nil {
		return b.ctx.Err()
	}
	var envelope struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error  struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if decodeErr := json.Unmarshal(stdout.Bytes(), &envelope); decodeErr != nil {
		return fmt.Errorf("mas %s: %v (invalid response: %v): %s", args[0], err, decodeErr, stderr.String())
	}
	if err != nil || !envelope.OK {
		return fmt.Errorf("mas %s rejected: %s (%v)", args[0], envelope.Error.Message, err)
	}
	if output != nil {
		if err := json.Unmarshal(envelope.Result, output); err != nil {
			return err
		}
	}
	switch args[0] {
	case "init":
		return b.snapshot("init", "", "")
	case "send":
		return b.snapshot("send", args[2], "")
	case "receive":
		return b.snapshot("receive", args[3], "")
	case "ack":
		return b.snapshot("ack", args[4], args[3])
	}
	return nil
}

func (b *bus) send(m message) error {
	var encoded struct {
		Text string `json:"text"`
	}
	if err := b.command(m, &encoded, "encode", "-"); err != nil {
		return err
	}
	// The native codec owns escaping and wire validation.
	return b.execute(strings.NewReader(encoded.Text), nil, "send", b.database, m.Sender, "-")
}

func (b *bus) begin(from, to, operation string, input any) (message, error) {
	b.next++
	m := message{
		ID: fmt.Sprintf("q%d", b.next), Run: "rollout", Sender: from, Recipient: to,
		Version: "1", Kind: "Request",
		Payload: map[string]any{"task_id": fmt.Sprintf("t%d", b.next), "operation": operation, "input": input},
	}
	return m, b.send(m)
}

func (b *bus) consume(expected message) (map[string]any, error) {
	var deliveries []struct {
		Message    message `json:"message"`
		Actionable bool    `json:"actionable"`
	}
	if err := b.command(nil, &deliveries, "receive", b.database, expected.Run, expected.Recipient, "1"); err != nil {
		return nil, err
	}
	if len(deliveries) != 1 {
		return nil, fmt.Errorf("expected delivery %s to %s", expected.ID, expected.Recipient)
	}
	// Normalize application structs and JSON numbers before comparing the full envelope.
	wire, err := json.Marshal(expected)
	if err != nil {
		return nil, err
	}
	var normalized message
	if err := json.Unmarshal(wire, &normalized); err != nil {
		return nil, err
	}
	delivery := deliveries[0]
	if !reflect.DeepEqual(delivery.Message, normalized) {
		return nil, fmt.Errorf("unexpected delivery for %s", expected.ID)
	}
	if expected.Kind == "Request" && !delivery.Actionable {
		return nil, fmt.Errorf("request %s is no longer actionable", expected.ID)
	}
	if err := b.command(nil, nil, "ack", b.database, expected.Run, expected.ID, expected.Recipient); err != nil {
		return nil, err
	}
	return delivery.Message.Payload, nil
}

func (b *bus) finish(request message, output any) (json.RawMessage, error) {
	reply := message{
		ID: "r" + request.ID[1:], Run: request.Run, Sender: request.Recipient,
		Recipient: request.Sender, Version: "1", Kind: "Result",
		Payload: map[string]any{
			"task_id": request.Payload["task_id"], "reply_to": request.ID,
			"status": "completed", "output": output,
		},
	}
	if err := b.send(reply); err != nil {
		return nil, err
	}
	delivered, err := b.consume(reply)
	if err != nil {
		return nil, err
	}
	if err := b.snapshot("result-delivered", reply.Recipient, reply.ID); err != nil {
		return nil, err
	}
	return json.Marshal(delivered["output"])
}

func decodeInput[T any](payload map[string]any) (T, error) {
	var input T
	data, err := json.Marshal(payload["input"])
	if err == nil {
		err = json.Unmarshal(data, &input)
	}
	return input, err
}

func exchange[I, O any](b *bus, from, to, operation string, input I, compute func(I) (O, error)) (O, error) {
	var result O
	request, err := b.begin(from, to, operation, input)
	if err != nil {
		return result, err
	}
	payload, err := b.consume(request)
	if err != nil {
		return result, err
	}
	delivered, err := decodeInput[I](payload)
	if err != nil {
		return result, err
	}
	output, err := compute(delivered)
	if err != nil {
		return result, err
	}
	raw, err := b.finish(request, output)
	if err != nil {
		return result, err
	}
	err = json.Unmarshal(raw, &result)
	return result, err
}
