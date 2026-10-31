package runtime

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/BingyanStudio/CloudPilot/internal/engine"
	"github.com/BingyanStudio/CloudPilot/internal/model"
)

type rpcReply struct {
	result json.RawMessage
	err    error
}
type rpcEnvelope struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
	CloudpilotReady bool `json:"cloudpilotReady"`
}

type appServer struct {
	input   io.Writer
	writeMu sync.Mutex
	mu      sync.Mutex
	next    uint64
	pending map[string]chan rpcReply
	ready   chan struct{}
	done    chan struct{}
	err     error
	notify  func(string, json.RawMessage)
	log     func(string, string)
}

func connectAppServer(input io.Writer, output io.Reader, notify func(string, json.RawMessage), log func(string, string)) *appServer {
	client := &appServer{input: input, pending: map[string]chan rpcReply{}, ready: make(chan struct{}), done: make(chan struct{}), notify: notify, log: log}
	go client.read(output)
	return client
}

func (c *appServer) read(output io.Reader) {
	defer close(c.done)
	scanner := bufio.NewScanner(output)
	scanner.Buffer(make([]byte, 64<<10), 8<<20)
	ready := false
	for scanner.Scan() {
		var message rpcEnvelope
		if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
			c.log("stdout", scanner.Text())
			continue
		}
		if message.CloudpilotReady {
			if !ready {
				close(c.ready)
				ready = true
			}
			continue
		}
		if !ready {
			c.log("stdout", scanner.Text())
			continue
		}
		if message.Method != "" {
			if len(message.ID) > 0 && string(message.ID) != "null" {
				_ = c.write(map[string]any{"jsonrpc": "2.0", "id": message.ID, "error": map[string]any{"code": -32000, "message": "This CloudPilot session does not support interactive approval or tool-input requests. Use the conversation to supply instructions."}})
				c.log("system", "Agent requested unsupported interaction: "+message.Method)
				c.notify("cloudpilot/unsupportedRequest", json.RawMessage(`{"message":"Agent requested an unsupported approval or tool-input interaction"}`))
			} else {
				c.notify(message.Method, message.Params)
			}
			continue
		}
		if len(message.ID) == 0 {
			continue
		}
		c.mu.Lock()
		pending := c.pending[string(message.ID)]
		delete(c.pending, string(message.ID))
		c.mu.Unlock()
		if pending != nil {
			reply := rpcReply{result: message.Result}
			if message.Error != nil {
				reply.err = errors.New(message.Error.Message)
			}
			pending <- reply
		}
	}
	c.mu.Lock()
	c.err = scanner.Err()
	if c.err == nil {
		c.err = io.EOF
	}
	c.mu.Unlock()
}

func (c *appServer) write(value any) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = c.input.Write(data)
	return err
}

func (c *appServer) call(ctx context.Context, method string, params any, result any) error {
	c.mu.Lock()
	c.next++
	id := c.next
	key := fmt.Sprint(id)
	reply := make(chan rpcReply, 1)
	c.pending[key] = reply
	c.mu.Unlock()
	defer func() { c.mu.Lock(); delete(c.pending, key); c.mu.Unlock() }()
	if err := c.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return err
	}
	select {
	case response := <-reply:
		if response.err != nil {
			return response.err
		}
		if result != nil {
			return json.Unmarshal(response.result, result)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		select {
		case response := <-reply:
			if response.err != nil {
				return response.err
			}
			if result != nil {
				return json.Unmarshal(response.result, result)
			}
			return nil
		default:
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		return fmt.Errorf("app-server connection ended: %w", c.err)
	}
}

type completedTurn struct{ id, status, message string }
type sessionObserver struct {
	mu               sync.Mutex
	threadID, turnID string
	emit             func(engine.SessionUpdate)
	redact           func(string) string
	secrets          []string
	tails            map[string]string
	summaryText      string
	completed        chan completedTurn
}

func (o *sessionObserver) setThread(id string) { o.mu.Lock(); o.threadID = id; o.mu.Unlock() }
func (o *sessionObserver) setTurn(id string)   { o.mu.Lock(); o.turnID = id; o.mu.Unlock() }

func (o *sessionObserver) event(event model.SessionEvent) {
	event.Text = o.redact(event.Text)
	event.Command = o.redact(event.Command)
	event.Output = o.redact(event.Output)
	event.Tool = o.redact(event.Tool)
	event.ItemID = o.redact(event.ItemID)
	o.emit(engine.SessionUpdate{Event: &event})
}

// safeDelta withholds a suffix that could become a credential when the next chunk arrives.
func (o *sessionObserver) safeDelta(key, delta string) string {
	value := o.redact(o.tails[key] + delta)
	hold := 0
	for _, secret := range o.secrets {
		for size := min(len(secret)-1, len(value)); size > hold; size-- {
			if strings.HasPrefix(secret, value[len(value)-size:]) {
				hold = size
				break
			}
		}
	}
	o.tails[key] = value[len(value)-hold:]
	return value[:len(value)-hold]
}

func (o *sessionObserver) notification(method string, raw json.RawMessage) {
	var data struct {
		ThreadID string `json:"threadId"`
		TurnID   string `json:"turnId"`
		ItemID   string `json:"itemId"`
		Delta    string `json:"delta"`
		Message  string `json:"message"`
		Error    *struct {
			Message string `json:"message"`
		} `json:"error"`
		WillRetry bool `json:"willRetry"`
		Turn      struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		} `json:"turn"`
		Item struct {
			ID, Type, Text, Command, Status, AggregatedOutput, Server, Tool, Query string
			ExitCode                                                               *int            `json:"exitCode"`
			Arguments                                                              json.RawMessage `json:"arguments"`
			Result                                                                 json.RawMessage `json:"result"`
			Error                                                                  json.RawMessage `json:"error"`
			Changes                                                                json.RawMessage `json:"changes"`
			Namespace                                                              string          `json:"namespace"`
			ContentItems                                                           json.RawMessage `json:"contentItems"`
			Success                                                                *bool           `json:"success"`
			Results                                                                json.RawMessage `json:"results"`
		} `json:"item"`
	}
	if json.Unmarshal(raw, &data) != nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if data.ThreadID != "" && o.threadID != "" && data.ThreadID != o.threadID {
		return
	}
	switch method {
	case "turn/started":
		o.turnID = data.Turn.ID
		o.emit(engine.SessionUpdate{ProviderTurnID: o.turnID})
	case "turn/completed":
		if o.turnID == "" || data.Turn.ID != o.turnID {
			return
		}
		completed := completedTurn{id: data.Turn.ID, status: data.Turn.Status}
		if data.Turn.Error != nil {
			completed.message = o.redact(data.Turn.Error.Message)
		}
		select {
		case o.completed <- completed:
		default:
		}
	case "item/agentMessage/delta":
		text := o.safeDelta("message:"+data.ItemID, data.Delta)
		if text != "" {
			o.event(model.SessionEvent{Type: "message", Role: "assistant", ItemID: data.ItemID, Text: text, Delta: true, Status: "streaming"})
		}
	case "item/commandExecution/outputDelta":
		text := o.safeDelta("tool:"+data.ItemID, data.Delta)
		if text != "" {
			o.event(model.SessionEvent{Type: "tool", ItemID: data.ItemID, Output: text, Delta: true, Status: "inProgress"})
		}
	case "item/started", "item/completed":
		item := data.Item
		status := "inProgress"
		if method == "item/completed" {
			status = "completed"
		}
		if item.Status == "failed" || item.Status == "declined" {
			status = "failed"
		}
		switch item.Type {
		case "agentMessage":
			if method == "item/completed" {
				o.summaryText = o.redact(item.Text)
				delete(o.tails, "message:"+item.ID)
				o.event(model.SessionEvent{Type: "message", Role: "assistant", ItemID: item.ID, Text: item.Text, Status: "completed"})
			}
		case "commandExecution":
			if item.ExitCode != nil && *item.ExitCode != 0 {
				status = "failed"
			}
			if method == "item/completed" {
				delete(o.tails, "tool:"+item.ID)
			}
			o.event(model.SessionEvent{Type: "tool", ItemID: item.ID, Tool: "shell", Command: item.Command, Output: item.AggregatedOutput, Status: status})
		case "mcpToolCall", "dynamicToolCall":
			output := o.safeJSON(item.Result)
			tool := strings.TrimPrefix(item.Server+"/"+item.Tool, "/")
			if item.Type == "dynamicToolCall" {
				tool = strings.TrimPrefix(item.Namespace+"/"+item.Tool, "/")
				output = o.safeJSON(item.ContentItems)
				if item.Success != nil && !*item.Success {
					status = "failed"
				}
			}
			if len(item.Error) > 0 && string(item.Error) != "null" {
				output = o.safeJSON(item.Error)
				status = "failed"
			}
			o.event(model.SessionEvent{Type: "tool", ItemID: item.ID, Tool: tool, Command: o.safeJSON(item.Arguments), Output: output, Status: status})
		case "fileChange":
			o.event(model.SessionEvent{Type: "tool", ItemID: item.ID, Tool: "fileChange", Output: o.safeJSON(item.Changes), Status: status})
		case "webSearch":
			o.event(model.SessionEvent{Type: "tool", ItemID: item.ID, Tool: "webSearch", Command: item.Query, Output: o.safeJSON(item.Results), Status: status})
		}
	case "item/mcpToolCall/progress":
		if text := o.safeDelta("tool:"+data.ItemID, data.Message); text != "" {
			o.event(model.SessionEvent{Type: "tool", ItemID: data.ItemID, Output: text + "\n", Delta: true, Status: "inProgress"})
		}
	case "error", "cloudpilot/unsupportedRequest":
		message := data.Message
		if data.Error != nil {
			message = data.Error.Message
		}
		eventType, status := "error", "failed"
		if data.WillRetry {
			eventType, status = "status", "retrying"
		}
		o.event(model.SessionEvent{Type: eventType, Role: "system", Text: message, Status: status})
	}
}

func (o *sessionObserver) summary() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.summaryText
}

func (o *sessionObserver) safeJSON(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return o.redact(string(raw))
	}
	var scrub func(any) any
	scrub = func(value any) any {
		switch typed := value.(type) {
		case string:
			return o.redact(typed)
		case []any:
			for i, item := range typed {
				typed[i] = scrub(item)
			}
		case map[string]any:
			for key, item := range typed {
				delete(typed, key)
				typed[o.redact(key)] = scrub(item)
			}
		}
		return value
	}
	encoded, _ := json.MarshalIndent(scrub(value), "", "  ")
	return string(encoded)
}

func rpcContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, 30*time.Second)
}
