package cluster

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// ErrNodeURLTakeover is returned by Upsert when a heartbeat would re-point a live node's URL.
var ErrNodeURLTakeover = errors.New("node url takeover refused: another registration for this node id is still live at a different url")

// ErrNodeTLSPinTakeover is returned by Upsert when a heartbeat would replace a live node's TLS pin.
var ErrNodeTLSPinTakeover = errors.New("node tls pin takeover refused: another registration for this node id is still live with a different tls pin")

// ErrStaleHeartbeat is returned when a heartbeat's timestamp is not newer than the last accepted one.
var ErrStaleHeartbeat = errors.New("stale heartbeat refused: its timestamp is not newer than the last one accepted for this node")

// beatSkew stops a node with a clock far off from parking a timestamp that
// refuses every later beat. It equals orgassert.MaxSkew: a node the control
// plane accepts must also accept the control plane's signed calls, or it looks
// healthy while refusing all API work (a 2-minute skew did exactly that at 5).
const beatSkew = 2 * time.Minute

// NodeFlag is an operator-set scheduling exclusion.
type NodeFlag string

const (
	FlagCordoned NodeFlag = "cordoned"
	FlagDraining NodeFlag = "draining"
)

// NodeUpsert is what one heartbeat asserts. Not a *Node because a heartbeat
// may not set FirstSeen, the URL-churn counters or the operator flags.
type NodeUpsert struct {
	ID        string
	URL       string
	Key       string
	Zone      string
	Labels    map[string]string
	Version   string
	Runtime   string
	Isolation bool
	Templates []string
	// CanFetchTemplates reports a shared artifact store; see Node.
	CanFetchTemplates bool
	// AutoPause reports that idle sandboxes are paused on the node; see Node.
	AutoPause bool
	Cap       Capacity
	// TLSSPKI is empty from a node that predates the transport change; it keeps
	// working over plaintext until PILLDROPS_REQUIRE_SECURE_HOP is set.
	TLSSPKI string
	// BeatMS is the node's own clock in Unix milliseconds, zero from a
	// pre-upgrade node. Milliseconds because a restarting node beats within the
	// same second as its previous one.
	BeatMS int64
}

// NodeMeta is what a heartbeat says about the node's runtime, as opposed to its
// identity and capacity. A struct rather than positional arguments: the flags
// are all bools and a transposed pair would place tenants on the wrong node.
type NodeMeta struct {
	Runtime   string
	Isolation bool
	// Templates holds nil for "does not enumerate, boots anything".
	Templates []string
	CanFetch  bool
	AutoPause bool
	// TagNetworks reports that the node's runtime can partition sandboxes into
	// opt-in network tags; a create naming tags is placed only on such a node.
	TagNetworks bool
	// EgressDomains reports that the node runs the name layer, so a rule set
	// naming a domain is enforceable there; a create carrying one is placed
	// only on such a node. Narrower than "enforces an egress policy": address
	// and CIDR rules need only the kernel chain, which every netns node has.
	EgressDomains bool
	// Volumes reports that the node can create and attach standalone volumes.
	Volumes bool
	// Checkpoint, NamedSnapshot and Standby are the capture-plane capabilities.
	// NamedSnapshot is the narrowest on firecracker — snapshots and per-sandbox
	// namespaces both — and is what a create from a snapshot is placed on.
	Checkpoint    bool
	NamedSnapshot bool
	Standby       bool
	// MultiPort, L4Forward and NestedDocker are reported for the fleet
	// capability answer only; nothing is placed on them.
	MultiPort    bool
	L4Forward    bool
	NestedDocker bool
	// CPUFeatures is the node's dispatch-relevant instruction set, for placing
	// a memory snapshot where its frozen code paths actually run. Nil means the
	// node did not say, which is not the same as a CPU with no features.
	CPUFeatures []string
}

// NodeStore is the fleet's node table plus its in-flight capacity reservations.
// Reads return nodes with the operator flags already merged in.
type NodeStore interface {
	Upsert(u NodeUpsert, now time.Time) error
	// SetMeta is separate from Upsert, so a node's first heartbeat has a brief
	// window with no template list, which reads as "unrestricted". Fold into
	// Upsert if that ever shows up.
	SetMeta(id string, m NodeMeta) error
	SetFlag(id string, f NodeFlag, on bool) (found bool, err error)
	Get(id string) (*Node, bool, error)
	List() ([]*Node, error)
	Delete(id string) (found bool, err error)

	// Reserve holds memMB on a node until expiry.
	Reserve(nodeID string, memMB int, expiry time.Time) error
	// Unreserve drops one hold of memMB; reservations are fungible by size. No-op if none match.
	Unreserve(nodeID string, memMB int) error
	// ReservedMB returns live holds fleet-wide as nodeID to MB, pruning expired ones.
	ReservedMB(now time.Time) (map[string]int, error)
}

func reservationToken() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// nodeRecord is the stored shape of a node, including the fields Node hides
// from clients with json:"-" (Key, TLSSPKI, FirstSeen, churn counters).
type nodeRecord struct {
	ID        string            `json:"id"`
	URL       string            `json:"url"`
	Key       string            `json:"key"`
	Zone      string            `json:"zone"`
	Labels    map[string]string `json:"labels,omitempty"`
	Version   string            `json:"version,omitempty"`
	Runtime   string            `json:"runtime,omitempty"`
	Isolation bool              `json:"isolation"`
	// Not omitempty: holdsTemplate reads nil as "boots anything" and [] as
	// "holds nothing", and omitempty would turn the second into the first.
	Templates       []string  `json:"templates"`
	CanFetch        bool      `json:"can_fetch_templates,omitempty"`
	AutoPause       bool      `json:"auto_pause,omitempty"`
	TagNetworks     bool      `json:"tag_networks,omitempty"`
	EgressDomains   bool      `json:"egress_domains,omitempty"`
	Volumes         bool      `json:"volumes,omitempty"`
	Checkpoint      bool      `json:"checkpoint,omitempty"`
	NamedSnapshot   bool      `json:"named_snapshot,omitempty"`
	Standby         bool      `json:"standby,omitempty"`
	MultiPort       bool      `json:"multiport,omitempty"`
	L4Forward       bool      `json:"l4_forward,omitempty"`
	NestedDocker    bool      `json:"nested_docker,omitempty"`
	CPUFeatures     []string  `json:"cpu_features,omitempty"`
	Cap             Capacity  `json:"capacity"`
	LastSeen        time.Time `json:"last_seen"`
	FirstSeen       time.Time `json:"first_seen"`
	URLChanges      int       `json:"url_changes,omitempty"`
	LastURLChangeAt time.Time `json:"last_url_change_at,omitempty"`
	// TLSSPKI is the transport pin; never serialized to a client.
	TLSSPKI string `json:"tls_spki,omitempty"`
	// LastBeatMS is persisted, not per-process, or a replayed beat would
	// succeed by landing on a different replica.
	LastBeatMS int64 `json:"last_beat_ms,omitempty"`
}

// meta and setMeta keep the heartbeat-reported half of a record in one place,
// so a new flag cannot be preserved across an Upsert in one store and dropped
// in the other.
func (r nodeRecord) meta() NodeMeta {
	return NodeMeta{Runtime: r.Runtime, Isolation: r.Isolation, Templates: r.Templates,
		CanFetch: r.CanFetch, AutoPause: r.AutoPause, TagNetworks: r.TagNetworks,
		EgressDomains: r.EgressDomains, Volumes: r.Volumes,
		Checkpoint: r.Checkpoint, NamedSnapshot: r.NamedSnapshot, Standby: r.Standby,
		MultiPort: r.MultiPort, L4Forward: r.L4Forward, NestedDocker: r.NestedDocker,
		CPUFeatures: r.CPUFeatures}
}

func (r *nodeRecord) setMeta(m NodeMeta) {
	r.Runtime, r.Isolation, r.Templates = m.Runtime, m.Isolation, m.Templates
	r.CanFetch, r.AutoPause, r.TagNetworks = m.CanFetch, m.AutoPause, m.TagNetworks
	r.EgressDomains, r.Volumes = m.EgressDomains, m.Volumes
	r.Checkpoint, r.NamedSnapshot, r.Standby = m.Checkpoint, m.NamedSnapshot, m.Standby
	r.MultiPort, r.L4Forward, r.NestedDocker = m.MultiPort, m.L4Forward, m.NestedDocker
	r.CPUFeatures = m.CPUFeatures
}

func (r nodeRecord) node(flags nodeFlags) *Node {
	return &Node{
		ID: r.ID, URL: r.URL, Key: r.Key, Zone: r.Zone, Labels: r.Labels,
		Version: r.Version, Runtime: r.Runtime, Isolation: r.Isolation,
		Templates: r.Templates, CanFetchTemplates: r.CanFetch, AutoPause: r.AutoPause,
		TagNetworks: r.TagNetworks, EgressDomains: r.EgressDomains, Volumes: r.Volumes, Checkpoint: r.Checkpoint,
		NamedSnapshot: r.NamedSnapshot, Standby: r.Standby,
		MultiPort: r.MultiPort, L4Forward: r.L4Forward, NestedDocker: r.NestedDocker,
		CPUFeatures: r.CPUFeatures,
		Cap:         r.Cap,
		TLSSPKI:     r.TLSSPKI,
		LastSeen:    r.LastSeen, FirstSeen: r.FirstSeen,
		Cordoned: flags.Cordoned, Draining: flags.Draining,
		URLChanges: r.URLChanges, LastURLChangeAt: r.LastURLChangeAt,
	}
}

type nodeFlags struct {
	Cordoned bool `json:"cordoned,omitempty"`
	Draining bool `json:"draining,omitempty"`
}

// SavedNodeFlags is one node's operator flags as kept outside the node table.
type SavedNodeFlags struct {
	NodeID             string
	Cordoned, Draining bool
}

func (f *nodeFlags) set(flag NodeFlag, on bool) {
	switch flag {
	case FlagCordoned:
		f.Cordoned = on
	case FlagDraining:
		f.Draining = on
	}
}

// applyHeartbeat folds one heartbeat into a record (prev is nil on first
// registration). A URL or TLS pin change is accepted only once the existing
// registration has aged past NodeTTL: a moved node goes stale, a hijacker's
// victim keeps its record fresh. A refused beat is not written, so it cannot
// refresh LastSeen and hold its own window shut. BeatMS and TLSSPKI are
// optional for pre-upgrade nodes, but present and wrong is always refused.
func applyHeartbeat(prev *nodeRecord, u NodeUpsert, now time.Time) (nodeRecord, error) {
	if u.BeatMS != 0 {
		skew := time.UnixMilli(u.BeatMS).Sub(now)
		if skew < -beatSkew || skew > beatSkew {
			return nodeRecord{}, fmt.Errorf("%w: node clock is %s from the control plane's", ErrStaleHeartbeat, skew.Round(time.Second))
		}
	}
	rec := nodeRecord{FirstSeen: now}
	if prev != nil {
		rec = *prev
		// Before the URL rule: a replayed beat carries the old URL and would
		// otherwise be reported as a takeover.
		if u.BeatMS != 0 && prev.LastBeatMS != 0 && u.BeatMS <= prev.LastBeatMS {
			return nodeRecord{}, fmt.Errorf("%w (last accepted %d, this beat %d)", ErrStaleHeartbeat, prev.LastBeatMS, u.BeatMS)
		}
		if prev.URL != "" && prev.URL != u.URL {
			if now.Sub(prev.LastSeen) < NodeTTL {
				return nodeRecord{}, ErrNodeURLTakeover
			}
			rec.URLChanges++
			rec.LastURLChangeAt = now
		}
		// Refuses a pin change, never a pin arriving: a just-upgraded node's
		// first pin has prev.TLSSPKI == "" and must go through.
		if prev.TLSSPKI != "" && u.TLSSPKI != "" && prev.TLSSPKI != u.TLSSPKI {
			if now.Sub(prev.LastSeen) < NodeTTL {
				return nodeRecord{}, ErrNodeTLSPinTakeover
			}
		}
	}
	// A missing pin never clears the stored one; that would be the plaintext
	// downgrade an on-path attacker wants.
	if u.TLSSPKI != "" {
		rec.TLSSPKI = u.TLSSPKI
	}
	if u.BeatMS != 0 {
		rec.LastBeatMS = u.BeatMS
	}
	rec.ID, rec.URL = u.ID, u.URL
	if u.Key != "" {
		rec.Key = u.Key
	}
	zone := u.Zone
	if zone == "" {
		zone = "default"
	}
	rec.Zone = zone
	rec.Labels, rec.Version, rec.Cap, rec.LastSeen = u.Labels, u.Version, u.Cap, now
	return rec, nil
}

type memReservation struct {
	mem    int
	expiry time.Time
}

type memNodeStore struct {
	mu    sync.RWMutex
	recs  map[string]*nodeRecord
	flags map[string]*nodeFlags
	resv  map[string][]memReservation
}

func newMemNodeStore() *memNodeStore {
	return &memNodeStore{
		recs:  map[string]*nodeRecord{},
		flags: map[string]*nodeFlags{},
		resv:  map[string][]memReservation{},
	}
}

func (m *memNodeStore) Upsert(u NodeUpsert, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, err := applyHeartbeat(m.recs[u.ID], u, now)
	if err != nil {
		return err
	}
	// Preserve SetMeta fields so an Upsert never blanks them.
	if prev := m.recs[u.ID]; prev != nil {
		rec.setMeta(prev.meta())
	}
	m.recs[u.ID] = &rec
	return nil
}

func (m *memNodeStore) SetMeta(id string, meta NodeMeta) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if rec, ok := m.recs[id]; ok {
		rec.setMeta(meta)
	}
	return nil
}

func (m *memNodeStore) SetFlag(id string, f NodeFlag, on bool) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.recs[id]; !ok {
		return false, nil
	}
	fl, ok := m.flags[id]
	if !ok {
		fl = &nodeFlags{}
		m.flags[id] = fl
	}
	fl.set(f, on)
	return true, nil
}

// seedFlags sets flags for nodes that have not sent a heartbeat yet, which
// SetFlag refuses: after a restart the table starts empty.
func (m *memNodeStore) seedFlags(saved []SavedNodeFlags) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range saved {
		m.flags[s.NodeID] = &nodeFlags{Cordoned: s.Cordoned, Draining: s.Draining}
	}
}

func (m *memNodeStore) flagsFor(id string) nodeFlags {
	if fl, ok := m.flags[id]; ok {
		return *fl
	}
	return nodeFlags{}
}

func (m *memNodeStore) Get(id string) (*Node, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	rec, ok := m.recs[id]
	if !ok {
		return nil, false, nil
	}
	return rec.node(m.flagsFor(id)), true, nil
}

func (m *memNodeStore) List() ([]*Node, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Node, 0, len(m.recs))
	for id, rec := range m.recs {
		out = append(out, rec.node(m.flagsFor(id)))
	}
	return out, nil
}

func (m *memNodeStore) Delete(id string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.recs[id]
	delete(m.recs, id)
	delete(m.flags, id)
	delete(m.resv, id)
	return ok, nil
}

func (m *memNodeStore) Reserve(nodeID string, memMB int, expiry time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.resv[nodeID] = append(m.resv[nodeID], memReservation{mem: memMB, expiry: expiry})
	return nil
}

func (m *memNodeStore) Unreserve(nodeID string, memMB int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	rs := m.resv[nodeID]
	for i, rv := range rs {
		if rv.mem == memMB {
			m.resv[nodeID] = append(rs[:i], rs[i+1:]...)
			if len(m.resv[nodeID]) == 0 {
				delete(m.resv, nodeID)
			}
			return nil
		}
	}
	return nil
}

func (m *memNodeStore) ReservedMB(now time.Time) (map[string]int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]int{}
	for id, rs := range m.resv {
		live := rs[:0]
		total := 0
		for _, rv := range rs {
			if rv.expiry.After(now) {
				live = append(live, rv)
				total += rv.mem
			}
		}
		if len(live) == 0 {
			delete(m.resv, id)
			continue
		}
		m.resv[id] = live
		if total > 0 {
			out[id] = total
		}
	}
	return out, nil
}

const (
	// nodesKey is a hash of node id to nodeRecord JSON.
	nodesKey = "sandforge:nodes"
	// nodeFlagsKey is kept apart from the records so a cordon and a heartbeat
	// cannot overwrite one another.
	nodeFlagsKey = "sandforge:nodeflags"
	// reservationsKey is one fleet-wide sorted set: member "<nodeID>|<memMB>|<token>",
	// score expiry in unix nanoseconds, so one read gives the scheduler every hold.
	reservationsKey = "sandforge:reservations"
)

type redisNodeStore struct {
	c *redis.Client
}

func NewRedisNodeStore(url string) (*redisNodeStore, error) {
	c, err := dialRedis(url)
	if err != nil {
		return nil, err
	}
	return &redisNodeStore{c: c}, nil
}

func (r *redisNodeStore) getRecord(ctx context.Context, id string) (*nodeRecord, bool, error) {
	raw, err := r.c.HGet(ctx, nodesKey, id).Result()
	if errors.Is(err, redis.Nil) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var rec nodeRecord
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		return nil, false, err
	}
	return &rec, true, nil
}

func (r *redisNodeStore) putRecord(ctx context.Context, rec nodeRecord) error {
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return r.c.HSet(ctx, nodesKey, rec.ID, b).Err()
}

func (r *redisNodeStore) Upsert(u NodeUpsert, now time.Time) error {
	ctx, cancel := rctx()
	defer cancel()
	prev, _, err := r.getRecord(ctx, u.ID)
	if err != nil {
		return err
	}
	rec, err := applyHeartbeat(prev, u, now)
	if err != nil {
		return err
	}
	if prev != nil {
		rec.setMeta(prev.meta())
	}
	return r.putRecord(ctx, rec)
}

func (r *redisNodeStore) SetMeta(id string, meta NodeMeta) error {
	ctx, cancel := rctx()
	defer cancel()
	rec, ok, err := r.getRecord(ctx, id)
	if err != nil || !ok {
		return err
	}
	rec.setMeta(meta)
	return r.putRecord(ctx, *rec)
}

func (r *redisNodeStore) SetFlag(id string, f NodeFlag, on bool) (bool, error) {
	ctx, cancel := rctx()
	defer cancel()
	if _, ok, err := r.getRecord(ctx, id); err != nil || !ok {
		return false, err
	}
	flags, err := r.flagsFor(ctx, id)
	if err != nil {
		return false, err
	}
	flags.set(f, on)
	b, err := json.Marshal(flags)
	if err != nil {
		return false, err
	}
	if err := r.c.HSet(ctx, nodeFlagsKey, id, b).Err(); err != nil {
		return false, err
	}
	return true, nil
}

func (r *redisNodeStore) flagsFor(ctx context.Context, id string) (nodeFlags, error) {
	var f nodeFlags
	raw, err := r.c.HGet(ctx, nodeFlagsKey, id).Result()
	if errors.Is(err, redis.Nil) {
		return f, nil
	}
	if err != nil {
		return f, err
	}
	_ = json.Unmarshal([]byte(raw), &f)
	return f, nil
}

func (r *redisNodeStore) Get(id string) (*Node, bool, error) {
	ctx, cancel := rctx()
	defer cancel()
	rec, ok, err := r.getRecord(ctx, id)
	if err != nil || !ok {
		return nil, false, err
	}
	flags, err := r.flagsFor(ctx, id)
	if err != nil {
		return nil, false, err
	}
	return rec.node(flags), true, nil
}

func (r *redisNodeStore) List() ([]*Node, error) {
	ctx, cancel := rctx()
	defer cancel()
	recs, err := r.c.HGetAll(ctx, nodesKey).Result()
	if err != nil {
		return nil, err
	}
	rawFlags, err := r.c.HGetAll(ctx, nodeFlagsKey).Result()
	if err != nil {
		return nil, err
	}
	flags := make(map[string]nodeFlags, len(rawFlags))
	for id, raw := range rawFlags {
		var f nodeFlags
		if json.Unmarshal([]byte(raw), &f) == nil {
			flags[id] = f
		}
	}
	out := make([]*Node, 0, len(recs))
	for id, raw := range recs {
		var rec nodeRecord
		if json.Unmarshal([]byte(raw), &rec) != nil {
			continue
		}
		out = append(out, rec.node(flags[id]))
	}
	return out, nil
}

func (r *redisNodeStore) Delete(id string) (bool, error) {
	ctx, cancel := rctx()
	defer cancel()
	n, err := r.c.HDel(ctx, nodesKey, id).Result()
	if err != nil {
		return false, err
	}
	_ = r.c.HDel(ctx, nodeFlagsKey, id).Err()
	if members, mErr := r.reservationMembers(ctx); mErr == nil {
		var stale []any
		for _, m := range members {
			if node, _, _, ok := parseReservation(m); ok && node == id {
				stale = append(stale, m)
			}
		}
		if len(stale) > 0 {
			_ = r.c.ZRem(ctx, reservationsKey, stale...).Err()
		}
	}
	return n > 0, nil
}

func (r *redisNodeStore) reservationMembers(ctx context.Context) ([]string, error) {
	return r.c.ZRange(ctx, reservationsKey, 0, -1).Result()
}

// reservationMember encodes one hold; the score is the expiry so pruning is a single range delete.
func reservationMember(nodeID string, memMB int) string {
	return nodeID + "|" + strconv.Itoa(memMB) + "|" + reservationToken()
}

func parseReservation(member string) (nodeID string, memMB int, token string, ok bool) {
	parts := strings.SplitN(member, "|", 3)
	if len(parts) != 3 {
		return "", 0, "", false
	}
	mb, err := strconv.Atoi(parts[1])
	if err != nil {
		return "", 0, "", false
	}
	return parts[0], mb, parts[2], true
}

func (r *redisNodeStore) Reserve(nodeID string, memMB int, expiry time.Time) error {
	ctx, cancel := rctx()
	defer cancel()
	return r.c.ZAdd(ctx, reservationsKey, redis.Z{
		Score:  float64(expiry.UnixNano()),
		Member: reservationMember(nodeID, memMB),
	}).Err()
}

func (r *redisNodeStore) Unreserve(nodeID string, memMB int) error {
	ctx, cancel := rctx()
	defer cancel()
	members, err := r.reservationMembers(ctx)
	if err != nil {
		return err
	}
	for _, m := range members {
		if node, mb, _, ok := parseReservation(m); ok && node == nodeID && mb == memMB {
			return r.c.ZRem(ctx, reservationsKey, m).Err()
		}
	}
	return nil
}

func (r *redisNodeStore) ReservedMB(now time.Time) (map[string]int, error) {
	ctx, cancel := rctx()
	defer cancel()
	// Prune first so a hold taken by a replica that then died clears itself.
	if err := r.c.ZRemRangeByScore(ctx, reservationsKey, "-inf",
		"("+strconv.FormatInt(now.UnixNano(), 10)).Err(); err != nil {
		return nil, err
	}
	members, err := r.reservationMembers(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]int{}
	for _, m := range members {
		if node, mb, _, ok := parseReservation(m); ok {
			out[node] += mb
		}
	}
	return out, nil
}

// reset clears the fleet tables; test-only.
func (r *redisNodeStore) reset() error {
	ctx, cancel := rctx()
	defer cancel()
	return r.c.Del(ctx, nodesKey, nodeFlagsKey, reservationsKey).Err()
}

// sortNodes orders by id; callers treat position 0 as the primary, so map order will not do.
func sortNodes(ns []*Node) []*Node {
	sort.Slice(ns, func(i, j int) bool { return ns[i].ID < ns[j].ID })
	return ns
}
