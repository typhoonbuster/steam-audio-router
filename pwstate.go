package main

import (
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	typeNode   = "PipeWire:Interface:Node"
	typeClient = "PipeWire:Interface:Client"
	typeLink   = "PipeWire:Interface:Link"
	typePort   = "PipeWire:Interface:Port"
)

// pwRaw is one element of a pw-dump JSON array, as emitted on the wire.
// Removals are `{"id": N, "info": null}`. A few object kinds (metadata) have
// no "info" but top-level "props".
type pwRaw struct {
	ID    int             `json:"id"`
	Type  string          `json:"type"`
	Info  json.RawMessage `json:"info"`
	Props map[string]any  `json:"props"`
}

type pwInfo struct {
	ChangeMask   []string       `json:"change-mask"`
	State        string         `json:"state"`
	Direction    string         `json:"direction"` // ports: "input" / "output"
	Props        map[string]any `json:"props"`
	OutputNodeID *int           `json:"output-node-id"` // links
	OutputPortID *int           `json:"output-port-id"`
	InputNodeID  *int           `json:"input-node-id"`
	InputPortID  *int           `json:"input-port-id"`
}

// pwObject is our decoded view of a PipeWire global.
type pwObject struct {
	ID      int
	Type    string
	Removed bool
	Info    pwInfo
	Props   map[string]any // objects without "info"
}

// props returns the property map wherever pw-dump put it.
func (o *pwObject) props() map[string]any {
	if o.Info.Props != nil {
		return o.Info.Props
	}
	return o.Props
}

func decodeBatch(raw []pwRaw) []pwObject {
	out := make([]pwObject, 0, len(raw))
	for _, r := range raw {
		o := pwObject{ID: r.ID, Type: r.Type, Props: r.Props}
		if len(r.Info) > 0 {
			if string(r.Info) == "null" {
				o.Removed = true
			} else if err := json.Unmarshal(r.Info, &o.Info); err != nil {
				Debugf("pw-dump: cannot decode info of object %d: %v", r.ID, err)
			}
		}
		out = append(out, o)
	}
	return out
}

// propString renders a property as a string (JSON strings, numbers, bools).
func propString(props map[string]any, key string) (string, bool) {
	v, ok := props[key]
	if !ok || v == nil {
		return "", false
	}
	switch x := v.(type) {
	case string:
		return x, true
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64), true
	case bool:
		return strconv.FormatBool(x), true
	}
	return "", false
}

func propInt(props map[string]any, key string) (int, bool) {
	s, ok := propString(props, key)
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	return n, err == nil
}

var nonAlnum = regexp.MustCompile(`[^a-z0-9]`)

// normalizeName lowercases and strips everything but letters and digits, so
// "Steam Process-Recording" == "steam_process_recording".
func normalizeName(s string) string { return nonAlnum.ReplaceAllString(strings.ToLower(s), "") }

// PwState is a local mirror of the PipeWire globals, keyed by id.
type PwState struct{ Objects map[int]*pwObject }

func newPwState() *PwState { return &PwState{Objects: map[int]*pwObject{}} }

func (s *PwState) Clear() { s.Objects = map[int]*pwObject{} }

// Apply merges a pw-dump batch and returns the ids added, updated and removed.
func (s *PwState) Apply(batch []pwObject) (added, updated, removed []int) {
	for i := range batch {
		o := batch[i]
		if o.Removed {
			if _, ok := s.Objects[o.ID]; ok {
				delete(s.Objects, o.ID)
				removed = append(removed, o.ID)
			}
			continue
		}
		if _, ok := s.Objects[o.ID]; ok {
			updated = append(updated, o.ID)
		} else {
			added = append(added, o.ID)
		}
		s.Objects[o.ID] = &o
	}
	return
}

func (s *PwState) Get(id int) *pwObject { return s.Objects[id] }

// OfType returns objects of a type, sorted by id for deterministic logs.
func (s *PwState) OfType(t string) []*pwObject {
	var out []*pwObject
	for _, o := range s.Objects {
		if o.Type == t {
			out = append(out, o)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// portInfo is what we need to pair ports.
type portInfo struct {
	ID      int
	Name    string
	Channel string // audio.channel (FL, FR, MONO, ...), may be empty
	Index   int    // port.id, position inside the node
}

// Ports lists a node's non-monitor ports in one direction ("output" or
// "input"), ordered by position.
func (s *PwState) Ports(nodeID int, direction string) []portInfo {
	var out []portInfo
	for _, o := range s.Objects {
		if o.Type != typePort || o.Info.Direction != direction {
			continue
		}
		props := o.props()
		if nid, ok := propInt(props, "node.id"); !ok || nid != nodeID {
			continue
		}
		if mon, _ := propString(props, "port.monitor"); mon == "true" {
			continue
		}
		name, _ := propString(props, "port.name")
		ch, _ := propString(props, "audio.channel")
		idx, _ := propInt(props, "port.id")
		out = append(out, portInfo{ID: o.ID, Name: name, Channel: ch, Index: idx})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Index != out[j].Index {
			return out[i].Index < out[j].Index
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// linkInfo is an existing link between two ports.
type linkInfo struct {
	ID      int
	OutPort int
	InPort  int
}

// LinksBetween returns the links going from outNode to inNode.
func (s *PwState) LinksBetween(outNode, inNode int) []linkInfo {
	var res []linkInfo
	for _, o := range s.Objects {
		if o.Type != typeLink || o.Info.OutputNodeID == nil || o.Info.InputNodeID == nil {
			continue
		}
		if *o.Info.OutputNodeID != outNode || *o.Info.InputNodeID != inNode {
			continue
		}
		li := linkInfo{ID: o.ID}
		if o.Info.OutputPortID != nil {
			li.OutPort = *o.Info.OutputPortID
		}
		if o.Info.InputPortID != nil {
			li.InPort = *o.Info.InputPortID
		}
		res = append(res, li)
	}
	sort.Slice(res, func(i, j int) bool { return res[i].ID < res[j].ID })
	return res
}

// planLinks pairs stream output ports with target input ports: same
// audio.channel first; a single source port (mono) fans out to every target
// port; remaining target ports fall back to positional matching. Extra
// source channels (e.g. surround into a stereo target) are left unlinked.
func planLinks(src, dst []portInfo) [][2]portInfo {
	var pairs [][2]portInfo
	for i, d := range dst {
		var pick *portInfo
		if d.Channel != "" {
			for j := range src {
				if src[j].Channel == d.Channel {
					pick = &src[j]
					break
				}
			}
		}
		if pick == nil && len(src) == 1 {
			pick = &src[0]
		}
		if pick == nil && i < len(src) {
			pick = &src[i]
		}
		if pick != nil {
			pairs = append(pairs, [2]portInfo{*pick, d})
		}
	}
	return pairs
}
