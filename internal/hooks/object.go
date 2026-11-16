package hooks

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// object is a JSON object that keeps its keys in file order.
type object struct{ members []member }

type member struct {
	key   string
	value json.RawMessage
}

func parseObject(data []byte) (object, error) {
	var o object
	if len(bytes.TrimSpace(data)) == 0 {
		return o, nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return o, fmt.Errorf("%w: %w", ErrInvalidJSON, err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return o, fmt.Errorf("%w: top level is not an object", ErrInvalidJSON)
	}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return o, fmt.Errorf("%w: %w", ErrInvalidJSON, err)
		}
		key, _ := tok.(string)
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return o, fmt.Errorf("%w: %w", ErrInvalidJSON, err)
		}
		o.members = append(o.members, member{key, v})
	}
	if _, err := dec.Token(); err != nil {
		return o, fmt.Errorf("%w: %w", ErrInvalidJSON, err)
	}
	if dec.More() {
		return o, fmt.Errorf("%w: trailing data", ErrInvalidJSON)
	}
	if _, err := dec.Token(); err == nil {
		return o, fmt.Errorf("%w: trailing data", ErrInvalidJSON)
	}
	return o, nil
}

func (o *object) get(key string) (json.RawMessage, bool) {
	for _, m := range o.members {
		if m.key == key {
			return m.value, true
		}
	}
	return nil, false
}

func (o *object) set(key string, v json.RawMessage) {
	for i, m := range o.members {
		if m.key == key {
			o.members[i].value = v
			return
		}
	}
	o.members = append(o.members, member{key, v})
}

func (o *object) delete(key string) {
	out := o.members[:0]
	for _, m := range o.members {
		if m.key != key {
			out = append(out, m)
		}
	}
	o.members = out
}

func (o *object) object(key string) (object, error) {
	raw, ok := o.get(key)
	if !ok || string(bytes.TrimSpace(raw)) == "null" {
		return object{}, nil
	}
	sub, err := parseObject(raw)
	if err != nil {
		return sub, fmt.Errorf("%q: %w", key, err)
	}
	return sub, nil
}

func (o object) marshal() json.RawMessage {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, m := range o.members {
		if i > 0 {
			b.WriteByte(',')
		}
		k, _ := json.Marshal(m.key)
		b.Write(k)
		b.WriteByte(':')
		b.Write(m.value)
	}
	b.WriteByte('}')
	return b.Bytes()
}

func (o *object) groups(event string) ([]group, error) {
	raw, ok := o.get(event)
	if !ok {
		return nil, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("hooks.%s is not a list: %w", event, err)
	}
	var out []group
	for _, it := range items {
		var g group
		if err := json.Unmarshal(it, &g); err != nil {
			return nil, fmt.Errorf("hooks.%s: %w", event, err)
		}
		var hs []json.RawMessage
		var wrap struct {
			Hooks []json.RawMessage `json:"hooks"`
		}
		if json.Unmarshal(it, &wrap) == nil {
			hs = wrap.Hooks
		}
		for i := range g.Hooks {
			if i < len(hs) {
				g.Hooks[i].raw = hs[i]
			}
		}
		g.extra = it
		out = append(out, g)
	}
	return out, nil
}

func (o *object) setGroups(event string, groups []group) error {
	var b bytes.Buffer
	b.WriteByte('[')
	for i, g := range groups {
		if i > 0 {
			b.WriteByte(',')
		}
		if g.extra != nil {
			b.Write(g.extra)
			continue
		}
		enc, err := encodeGroup(g)
		if err != nil {
			return err
		}
		b.Write(enc)
	}
	b.WriteByte(']')
	o.set(event, b.Bytes())
	return nil
}

// encodeGroup re-encodes a changed group, keeping each remaining hook's
// original JSON (and any fields we do not model).
func encodeGroup(g group) ([]byte, error) {
	var hs []json.RawMessage
	for _, h := range g.Hooks {
		if h.raw != nil {
			hs = append(hs, h.raw)
			continue
		}
		enc, err := json.Marshal(struct {
			Type    string `json:"type"`
			Command string `json:"command"`
			Timeout int    `json:"timeout,omitempty"`
		}{h.Type, h.Command, h.Timeout})
		if err != nil {
			return nil, err
		}
		hs = append(hs, enc)
	}
	type out struct {
		Matcher string            `json:"matcher,omitempty"`
		Hooks   []json.RawMessage `json:"hooks"`
	}
	return json.Marshal(out{g.Matcher, hs})
}
