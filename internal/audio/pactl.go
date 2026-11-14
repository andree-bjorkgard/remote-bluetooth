package audio

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Runner runs an external command and returns its standard output.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// ExecRunner runs commands with os/exec. A failing command's standard
// error is included in the returned error.
type ExecRunner struct{}

// Run implements Runner.
func (ExecRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

const (
	holdSinkName  = "rbt_hold"
	configuredKey = "default.configured.audio.sink"
	metadataType  = "Spa:String:JSON"
)

// card is a PipeWire device as pw-dump shows it.
type card struct {
	ID            int
	Name          string         // device.name, such as bluez_card.00_0A_45_19_F3_A6
	ActiveProfile string         // "" if PipeWire reports none
	Profiles      map[string]int // profile name -> EnumProfile index
}

type sink struct {
	Index       int             `json:"index"`
	Name        string          `json:"name"`
	OwnerModule json.RawMessage `json:"owner_module"`
}

// owner returns the id of the module that created the sink, "" if unknown.
func (s sink) owner() string {
	return strings.Trim(string(s.OwnerModule), `"`)
}

type sinkInput struct {
	Index int `json:"index"`
	Sink  int `json:"sink"`
}

// pactl wraps the pactl, pw-metadata, pw-dump and pw-cli commands the
// Manager needs.
type pactl struct{ r Runner }

func (p pactl) listJSON(ctx context.Context, what string, v any) error {
	out, err := p.r.Run(ctx, "pactl", "-f", "json", "list", what)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(out, v); err != nil {
		return fmt.Errorf("audio: parse pactl list %s: %w", what, err)
	}
	return nil
}

// cards lists PipeWire's devices from pw-dump.
func (p pactl) cards(ctx context.Context) ([]card, error) {
	out, err := p.r.Run(ctx, "pw-dump")
	if err != nil {
		return nil, err
	}
	return parseDevices(out)
}

type profileParam struct {
	Index int    `json:"index"`
	Name  string `json:"name"`
}

// parseDevices picks the devices out of pw-dump's JSON array of objects.
func parseDevices(out []byte) ([]card, error) {
	var objs []struct {
		ID   int             `json:"id"`
		Type string          `json:"type"`
		Info json.RawMessage `json:"info"`
	}
	if err := json.Unmarshal(out, &objs); err != nil {
		return nil, fmt.Errorf("audio: parse pw-dump: %w", err)
	}
	var cards []card
	for _, o := range objs {
		if !strings.HasSuffix(o.Type, "Device") || len(o.Info) == 0 || string(o.Info) == "null" {
			continue
		}
		var info struct {
			Props struct {
				Name string `json:"device.name"`
			} `json:"props"`
			Params struct {
				EnumProfile []profileParam `json:"EnumProfile"`
				Profile     []profileParam `json:"Profile"`
			} `json:"params"`
		}
		if err := json.Unmarshal(o.Info, &info); err != nil {
			return nil, fmt.Errorf("audio: parse pw-dump device %d: %w", o.ID, err)
		}
		c := card{ID: o.ID, Name: info.Props.Name, Profiles: map[string]int{}}
		for _, e := range info.Params.EnumProfile {
			c.Profiles[e.Name] = e.Index
		}
		if len(info.Params.Profile) > 0 {
			c.ActiveProfile = info.Params.Profile[0].Name
		}
		cards = append(cards, c)
	}
	return cards, nil
}

func (p pactl) sinks(ctx context.Context) ([]sink, error) {
	var s []sink
	return s, p.listJSON(ctx, "sinks", &s)
}

func (p pactl) sinkInputs(ctx context.Context) ([]sinkInput, error) {
	var s []sinkInput
	return s, p.listJSON(ctx, "sink-inputs", &s)
}

func (p pactl) defaultSink(ctx context.Context) (string, error) {
	out, err := p.r.Run(ctx, "pactl", "get-default-sink")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// configuredSink reads default.configured.audio.sink. set is false when the
// key has no value.
func (p pactl) configuredSink(ctx context.Context) (value string, set bool, err error) {
	out, err := p.r.Run(ctx, "pw-metadata", "0", configuredKey)
	if err != nil {
		return "", false, err
	}
	value, set = parseConfiguredSink(out)
	return value, set, nil
}

var updateValueRe = regexp.MustCompile(`value:'(.*)' type:'`)

// parseConfiguredSink finds the value in pw-metadata's
// "update: id:0 key:'default.configured.audio.sink' value:'…' type:'…'" line.
func parseConfiguredSink(out []byte) (string, bool) {
	for line := range strings.Lines(string(out)) {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "update:") || !strings.Contains(line, "key:'"+configuredKey+"'") {
			continue
		}
		if m := updateValueRe.FindStringSubmatch(line); m != nil {
			return m[1], true
		}
	}
	return "", false
}

func (p pactl) setConfiguredSink(ctx context.Context, value string) error {
	_, err := p.r.Run(ctx, "pw-metadata", "0", configuredKey, value, metadataType)
	return err
}

func (p pactl) deleteConfiguredSink(ctx context.Context) error {
	_, err := p.r.Run(ctx, "pw-metadata", "-d", "0", configuredKey)
	return err
}

// loadHoldSink loads the null sink streams are parked on and returns its
// module id.
func (p pactl) loadHoldSink(ctx context.Context) (string, error) {
	out, err := p.r.Run(ctx, "pactl", "load-module", "module-null-sink",
		"sink_name="+holdSinkName, "sink_properties=device.description=Released-for-handoff")
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(out))
	if _, err := strconv.Atoi(id); err != nil {
		return "", fmt.Errorf("audio: unexpected load-module output %q", id)
	}
	return id, nil
}

func (p pactl) unloadModule(ctx context.Context, id string) error {
	_, err := p.r.Run(ctx, "pactl", "unload-module", id)
	return err
}

func (p pactl) setDefaultSink(ctx context.Context, name string) error {
	_, err := p.r.Run(ctx, "pactl", "set-default-sink", name)
	return err
}

func (p pactl) moveSinkInput(ctx context.Context, index int, sinkName string) error {
	_, err := p.r.Run(ctx, "pactl", "move-sink-input", strconv.Itoa(index), sinkName)
	return err
}

// setCardProfile switches the card to profile through PipeWire with
// save: false. pactl set-card-profile would save it, and WirePlumber would
// then bring that profile (even "off") back on every later connection.
func (p pactl) setCardProfile(ctx context.Context, name, profile string) error {
	cards, err := p.cards(ctx)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(cards, func(c card) bool { return c.Name == name })
	if i < 0 {
		return fmt.Errorf("%w: %s", ErrNoCard, name)
	}
	idx, ok := cards[i].Profiles[profile]
	if !ok {
		return fmt.Errorf("audio: card %s has no profile %q", name, profile)
	}
	_, err = p.r.Run(ctx, "pw-cli", "set-param", strconv.Itoa(cards[i].ID), "Profile",
		fmt.Sprintf("{ index: %d, save: false }", idx))
	return err
}
