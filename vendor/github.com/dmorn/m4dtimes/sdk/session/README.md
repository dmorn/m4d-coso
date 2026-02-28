# sdk/session

Append-only JSONL session recording for m4dtimes agents.
Compatible with [Pi/OpenClaw](https://github.com/badlogic/pi-mono) session format.

## Format

Each user gets one JSONL file: `<dir>/<userID>.jsonl`.

Every line is a JSON event:

```jsonl
{"type":"session","version":1,"id":"a1b2c3d4","userId":7756297856,"timestamp":"2026-02-24T23:14:59Z"}
{"type":"message","id":"e5f6a7b8","parentId":"a1b2c3d4","timestamp":"2026-02-24T23:15:01Z","message":{"role":"user","content":[{"type":"text","text":"Cosa ho oggi?"}]}}
{"type":"message","id":"f0e1d2c3","parentId":"e5f6a7b8","timestamp":"2026-02-24T23:15:03Z","message":{"role":"assistant","usage":{"input_tokens":3264,"output_tokens":110},"content":[{"type":"tool_use","tool_call":{"id":"toolu_01","name":"execute_sql","arguments":"..."}}]}}
{"type":"message","id":"b4a5c6d7","parentId":"f0e1d2c3","timestamp":"2026-02-24T23:15:03Z","message":{"role":"user","content":[{"type":"tool_result","tool_result":{"tool_call_id":"toolu_01","content":"id | room | type\n...","is_error":false}}]}}
{"type":"message","id":"c8d9e0f1","parentId":"b4a5c6d7","timestamp":"2026-02-24T23:15:05Z","message":{"role":"assistant","usage":{"input_tokens":3410,"output_tokens":85},"content":[{"type":"text","text":"Hai 3 stanze assegnate stamattina..."}]}}
```

### Event fields

| Field | Present on | Description |
|-------|-----------|-------------|
| `type` | all | `"session"` or `"message"` |
| `version` | session init | Always `1` |
| `id` | all | 8-hex random ID |
| `parentId` | message events | Links to previous event — forms a chain |
| `timestamp` | all | RFC3339 UTC |
| `userId` | session init | Telegram user ID |
| `message` | message events | Full `llm.Message` including role, content, and usage |

### Message roles

| Role | When |
|------|------|
| `user` | Inbound Telegram message |
| `assistant` | LLM text reply or tool call — carries `usage` (input/output tokens) |
| `user` (tool_result) | Tool execution results returned to the LLM |

## Usage

```go
import "github.com/dmorn/m4dtimes/sdk/session"

store, err := session.NewStore("./sessions")
if err != nil {
    log.Fatal(err)
}
defer store.Close()

// Pass to agent
agent.New(agent.Options{
    Session: store,
    // ...
})
```

The `sdk/agent` package wires `store.Record(userID, msg)` to `ContextManager.OnAppend`
automatically — no additional code required in the agent loop.

## Analysis examples

```bash
# Last 5 messages for user 7756297856
tail -5 sessions/7756297856.jsonl | python3 -c "
import json, sys
for line in sys.stdin:
    e = json.loads(line)
    if e['type'] == 'message':
        m = e['message']
        role = m['role']
        usage = m.get('usage', {})
        text = next((b['text'] for b in m['content'] if b.get('type')=='text'), '[tool]')
        print(f\"{e['timestamp'][:19]} [{role}] {text[:80]}  {usage}\")
"

# Total tokens consumed by a user
cat sessions/7756297856.jsonl | python3 -c "
import json, sys
inp = out = 0
for line in sys.stdin:
    e = json.loads(line)
    if e.get('type') == 'message':
        u = e['message'].get('usage') or {}
        inp += u.get('input_tokens', 0)
        out += u.get('output_tokens', 0)
print(f'input: {inp}  output: {out}  total: {inp+out}')
"

# All tool calls in a session
cat sessions/7756297856.jsonl | python3 -c "
import json, sys
for line in sys.stdin:
    e = json.loads(line)
    if e.get('type') != 'message': continue
    for block in e['message'].get('content', []):
        if block.get('type') == 'tool_use':
            tc = block['tool_call']
            print(f\"{e['timestamp'][:19]}  {tc['name']}  {tc['arguments'][:60]}\")
"
```

## Files

```
sdk/session/
├── event.go      # Event struct, sessionInitEvent(), messageEvent(), newID()
├── recorder.go   # Recorder — per-user file handle, mutex, parentId chain
└── store.go      # Store — lazy Recorder creation, map[int64]*Recorder
```
