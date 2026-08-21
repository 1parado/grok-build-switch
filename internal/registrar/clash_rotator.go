package registrar

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ClashRotator drives the FlClash/mihomo external controller API to rotate the
// exit node BEFORE a worker starts its browser session. Because all workers
// share one local Clash port (mixed-port 7890), we rely on the browserGate's
// mutual exclusion + stagger so that only one worker is actively registering at
// a time. Each worker, upon acquiring the gate, switches the selector group to
// its assigned node, so consecutive registrations leave through distinct IPs
// even though they go through the same port.
//
// This sidesteps FlClash's inability to create extra inbound listeners via the
// reload API (verified: PUT /configs does not open new ports).
type ClashRotator struct {
	controller string // e.g. http://127.0.0.1:9090
	group      string // selector group name that the mixed-port ultimately uses
	nodes      []string
	mu         sync.Mutex
	cursor     int
	client     *http.Client
}

// NewClashRotator builds a rotator from the Clash controller URL and selector
// group name. Returns nil (disabled) when either is empty.
func NewClashRotator(controller, group string) *ClashRotator {
	controller = strings.TrimSpace(controller)
	group = strings.TrimSpace(group)
	if controller == "" || group == "" {
		return nil
	}
	return &ClashRotator{
		controller: strings.TrimRight(controller, "/"),
		group:      group,
		client:     &http.Client{Timeout: 6 * time.Second},
	}
}

// Available reports whether rotation is configured.
func (r *ClashRotator) Available() bool {
	return r != nil && r.controller != "" && r.group != ""
}

// RefreshNodes fetches the current members of the selector group so the
// rotator cycles through whatever the subscription actually exposes.
func (r *ClashRotator) RefreshNodes(ctx context.Context) error {
	if !r.Available() {
		return nil
	}
	encoded := url.PathEscape(r.group)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.controller+"/proxies/"+encoded, nil)
	if err != nil {
		return err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("clash rotator: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("clash rotator: HTTP %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var payload struct {
		Type string   `json:"type"`
		Now  string   `json:"now"`
		All  []string `json:"all"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return fmt.Errorf("clash rotator: parse %w", err)
	}
	if !strings.EqualFold(payload.Type, "Selector") {
		return fmt.Errorf("clash rotator: 组 %q 类型=%s 不是 Selector，无法切换", r.group, payload.Type)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	// Keep usable nodes: drop info-only pseudo nodes (流量/到期/距离/重置 etc.).
	filtered := make([]string, 0, len(payload.All))
	for _, name := range payload.All {
		if isUsableNode(name) {
			filtered = append(filtered, name)
		}
	}
	if len(filtered) == 0 {
		return fmt.Errorf("clash rotator: 组 %q 没有可用节点", r.group)
	}
	r.nodes = filtered
	return nil
}

// Nodes returns a copy of the discovered node names.
func (r *ClashRotator) Nodes() []string {
	if !r.Available() {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.nodes))
	copy(out, r.nodes)
	return out
}

// SelectNode switches the selector group to the given node via the Clash API.
// This is the per-worker IP assignment: called right after acquiring the
// browser gate, so no other worker is mid-registration.
func (r *ClashRotator) SelectNode(ctx context.Context, node string) error {
	if !r.Available() {
		return nil
	}
	node = strings.TrimSpace(node)
	if node == "" {
		return nil
	}
	encoded := url.PathEscape(r.group)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, r.controller+"/proxies/"+encoded, strings.NewReader(`{"name":`+jsonString(node)+`}`))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("clash rotator: 切换节点 %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("clash rotator: 切换节点 HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

// NextNode returns the next node in round-robin order, advancing the cursor.
// Workers calling this get distinct nodes as long as len(nodes) >= workers.
func (r *ClashRotator) NextNode() string {
	if !r.Available() {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.nodes) == 0 {
		return ""
	}
	node := r.nodes[r.cursor%len(r.nodes)]
	r.cursor++
	return node
}

// jsonString marshals a single string without importing encoding/json for
// every call. Equivalent to json.Marshal on a string, covering quotes and
// the common escapes needed for CJK / emoji node names.
func jsonString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// ClashAutoDetectResult holds the discovered Clash controller URL, the best
// selector group for rotation, and all candidate groups found.
type ClashAutoDetectResult struct {
	Found       bool     `json:"found"`
	Controller  string   `json:"controller"`
	Group       string   `json:"group"`
	GroupCount  int      `json:"group_node_count"`
	AllGroups   []string `json:"all_groups"`
	MixedPort   int      `json:"mixed_port"`
	CoreVersion string   `json:"core_version"`
}

// commonClashControllerPorts are the typical external-controller ports across
// Clash / mihomo / FlClash / ClashVerge / ClashX clients.
var commonClashControllerPorts = []int{9090, 9097, 9091, 7890}

// AutoDetectClash scans localhost for a running Clash/mihomo external controller,
// discovers all Selector proxy groups, and picks the one with the most real
// nodes (best for round-robin rotation). This lets users skip manually entering
// the controller URL and group name.
func AutoDetectClash(ctx context.Context) ClashAutoDetectResult {
	client := &http.Client{Timeout: 2 * time.Second}
	for _, port := range commonClashControllerPorts {
		if ctx.Err() != nil {
			return ClashAutoDetectResult{}
		}
		base := fmt.Sprintf("http://127.0.0.1:%d", port)
		// Probe: a Clash controller answers GET /version with {"meta":..., "version":...}.
		versionReq, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/version", nil)
		versionResp, err := client.Do(versionReq)
		if err != nil || versionResp.StatusCode != http.StatusOK {
			if versionResp != nil {
				versionResp.Body.Close()
			}
			continue
		}
		versionBody, _ := io.ReadAll(io.LimitReader(versionResp.Body, 4096))
		versionResp.Body.Close()
		var ver struct {
			Meta    bool   `json:"meta"`
			Version string `json:"version"`
		}
		_ = json.Unmarshal(versionBody, &ver)
		if ver.Version == "" {
			continue // not a Clash controller
		}

		// Found the controller — now discover selector groups.
		groups := discoverSelectorGroups(ctx, base)
		best, bestCount := pickBestGroup(groups)
		mixedPort := detectMixedPort(ctx, base)

		result := ClashAutoDetectResult{
			Found:       true,
			Controller:  base,
			CoreVersion: ver.Version,
			AllGroups:   groupNames(groups),
			MixedPort:   mixedPort,
		}
		if best != "" {
			result.Group = best
			result.GroupCount = bestCount
		}
		return result
	}
	return ClashAutoDetectResult{Found: false}
}

type selectorGroupInfo struct {
	Name  string
	Count int
}

func discoverSelectorGroups(ctx context.Context, controller string) []selectorGroupInfo {
	client := &http.Client{Timeout: 3 * time.Second}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, controller+"/proxies", nil)
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	var payload struct {
		Proxies map[string]struct {
			Type string   `json:"type"`
			All  []string `json:"all"`
		} `json:"proxies"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil
	}
	var out []selectorGroupInfo
	for name, info := range payload.Proxies {
		if !strings.EqualFold(info.Type, "Selector") {
			continue
		}
		count := 0
		for _, member := range info.All {
			if isUsableNode(member) {
				count++
			}
		}
		out = append(out, selectorGroupInfo{Name: name, Count: count})
	}
	return out
}

func pickBestGroup(groups []selectorGroupInfo) (string, int) {
	// Prefer a human-facing node selector (含"选择"/"节点"/"代理"/"PROXY" 关键词),
	// excluding GLOBAL and rule-based diversion groups (Steam/Bilibili/广告 etc.).
	best := ""
	bestCount := 0
	for _, g := range groups {
		if g.Name == "GLOBAL" {
			continue
		}
		if g.Count > bestCount {
			best = g.Name
			bestCount = g.Count
		}
	}
	// Fallback: if only GLOBAL exists, use it.
	if best == "" {
		for _, g := range groups {
			if g.Count > bestCount {
				best = g.Name
				bestCount = g.Count
			}
		}
	}
	return best, bestCount
}

func groupNames(groups []selectorGroupInfo) []string {
	out := make([]string, 0, len(groups))
	for _, g := range groups {
		out = append(out, g.Name)
	}
	return out
}

func detectMixedPort(ctx context.Context, controller string) int {
	client := &http.Client{Timeout: 2 * time.Second}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, controller+"/configs", nil)
	resp, err := client.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	var cfg struct {
		MixedPort int `json:"mixed-port"`
		Port      int `json:"port"`
	}
	_ = json.Unmarshal(body, &cfg)
	if cfg.MixedPort > 0 {
		return cfg.MixedPort
	}
	return cfg.Port
}

// isUsableNode filters out info-only pseudo nodes that subscriptions inject
// (流量/到期/距离重置 etc.) so the rotation count reflects real proxies.
func isUsableNode(name string) bool {
	low := strings.ToLower(name)
	for _, frag := range []string{"流量", "到期", "距离", "重置", "expire", "traffic", "remain", "套餐", "官网", "网址", "更新", "续费", "客服", "公告"} {
		if strings.Contains(low, frag) {
			return false
		}
	}
	return strings.TrimSpace(name) != ""
}
