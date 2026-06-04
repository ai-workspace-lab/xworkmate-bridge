package acp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"xworkmate-bridge/internal/shared"
)

const (
	distributedForwardedHeader = "X-XWorkmate-Bridge-Forwarded"
	distributedSourceHeader    = "X-XWorkmate-Forward-Source"
	distributedTargetHeader    = "X-XWorkmate-Forward-Target"
	distributedTraceHeader     = "X-XWorkmate-Forward-Trace"
	distributedHopHeader       = "X-XWorkmate-Forward-Hop"

	defaultDistributedHopLimit = 3
	defaultSessionRouteTTL     = 24 * time.Hour
)

type distributedTaskRouterConfig struct {
	Config *BridgeConfig
	Token  string
}

type distributedTaskRouter struct {
	localNodeID string
	token       string
	hopLimit    int
	nodes       map[string]DistributedNodeConfig
	rules       []DistributedForwardRuleConfig
	routes      map[string]string
	routeStore  *distributedSessionRouteStore
	roundRobin  map[string]int
	httpClient  *http.Client
	mu          sync.Mutex
}

type distributedForwardDecision struct {
	targetNodeID string
	nextHopID    string
	endpoint     string
}

type distributedSessionRoute struct {
	TargetNodeID string
	ExpiresAt    time.Time
}

type distributedSessionRouteStore struct {
	mu     sync.Mutex
	routes map[string]distributedSessionRoute
	ttl    time.Duration
}

func newDistributedTaskRouter(config distributedTaskRouterConfig) *distributedTaskRouter {
	if config.Config == nil {
		return nil
	}
	distributed := config.Config.Distributed
	localNodeID := strings.TrimSpace(distributed.LocalNodeID)
	if localNodeID == "" {
		return nil
	}
	nodes := distributedNodeCatalog(distributed.Nodes)
	if _, ok := nodes[localNodeID]; !ok {
		return nil
	}
	rules := distributedForwardRules(distributed)
	if len(rules) == 0 {
		return nil
	}
	hopLimit := distributed.Forwarding.HopLimit
	if hopLimit <= 0 {
		hopLimit = defaultDistributedHopLimit
	}
	return &distributedTaskRouter{
		localNodeID: localNodeID,
		token:       strings.TrimSpace(config.Token),
		hopLimit:    hopLimit,
		nodes:       nodes,
		rules:       rules,
		routes:      distributedRouteMap(distributed.Forwarding.Routes),
		routeStore:  newDistributedSessionRouteStore(defaultSessionRouteTTL),
		roundRobin:  make(map[string]int),
		httpClient: shared.NewHTTPClient(openClawAgentWaitMaxTimeout + openClawAgentWaitHTTPMargin),
	}
}

func distributedNodeCatalog(configured []DistributedNodeConfig) map[string]DistributedNodeConfig {
	nodes := make(map[string]DistributedNodeConfig)
	for _, node := range defaultDistributedNodes() {
		if id := strings.TrimSpace(node.ID); id != "" {
			node.ID = id
			nodes[id] = node
		}
	}
	for _, node := range configured {
		if id := strings.TrimSpace(node.ID); id != "" {
			node.ID = id
			nodes[id] = node
		}
	}
	return nodes
}

func distributedForwardRules(distributed DistributedConfig) []DistributedForwardRuleConfig {
	if len(distributed.Forwarding.Rules) > 0 {
		return distributed.Forwarding.Rules
	}
	if peerID := strings.TrimSpace(distributed.TaskForwardPeerID); peerID != "" {
		return []DistributedForwardRuleConfig{
			{
				Methods: []string{"session.start", "session.message"},
				Target: DistributedForwardTargetConfig{
					NodeID: peerID,
				},
			},
		}
	}
	return nil
}

func distributedRouteMap(routes []DistributedRouteConfig) map[string]string {
	result := make(map[string]string)
	for _, route := range routes {
		target := strings.TrimSpace(route.TargetNodeID)
		nextHop := strings.TrimSpace(route.NextHopNodeID)
		if target != "" && nextHop != "" && target != nextHop {
			result[target] = nextHop
		}
	}
	return result
}

func (r *distributedTaskRouter) forward(ctx context.Context, w http.ResponseWriter, req *http.Request, request shared.RPCRequest) bool {
	decision, ok, err := r.forwardDecision(req, request)
	if err != nil {
		shared.WriteJSONError(w, request.ID, http.StatusBadGateway, -32060, err.Error())
		return true
	}
	if !ok {
		return false
	}
	payload, err := json.Marshal(request)
	if err != nil {
		shared.WriteJSONError(w, request.ID, http.StatusInternalServerError, -32603, "TASK_FORWARD_ENCODE_FAILED")
		return true
	}
	forwardURL, err := distributedForwardURL(decision.endpoint, req.URL.Path)
	if err != nil {
		shared.WriteJSONError(w, request.ID, http.StatusBadGateway, -32060, err.Error())
		return true
	}
	outbound, err := http.NewRequestWithContext(ctx, http.MethodPost, forwardURL, bytes.NewReader(payload))
	if err != nil {
		shared.WriteJSONError(w, request.ID, http.StatusBadGateway, -32060, "TASK_FORWARD_REQUEST_BUILD_FAILED: "+err.Error())
		return true
	}
	r.copyForwardRequestHeaders(outbound.Header, req.Header, request, decision)

	response, err := r.httpClient.Do(outbound)
	if err != nil {
		shared.WriteJSONError(w, request.ID, http.StatusBadGateway, -32060, "TASK_FORWARD_FAILED: "+err.Error())
		return true
	}
	defer func() { _ = response.Body.Close() }()
	copyForwardResponseHeaders(w.Header(), response.Header)
	w.WriteHeader(response.StatusCode)
	_, _ = io.Copy(w, response.Body)
	return true
}

func (r *distributedTaskRouter) forwardDecision(req *http.Request, request shared.RPCRequest) (distributedForwardDecision, bool, error) {
	if r == nil || req == nil {
		return distributedForwardDecision{}, false, nil
	}
	forwardedTarget := strings.TrimSpace(req.Header.Get(distributedTargetHeader))
	if strings.TrimSpace(req.Header.Get(distributedForwardedHeader)) != "" && forwardedTarget == "" {
		return distributedForwardDecision{}, false, nil
	}
	if forwardedTarget != "" {
		if strings.EqualFold(forwardedTarget, r.localNodeID) {
			return distributedForwardDecision{}, false, nil
		}
		return r.decisionForTarget(forwardedTarget, req)
	}

	method := strings.TrimSpace(request.Method)
	if !distributedForwardableMethod(method) {
		return distributedForwardDecision{}, false, nil
	}
	sessionKey := distributedSessionRouteKey(request)
	if sessionKey != "" {
		if routed, ok := r.routeStore.get(sessionKey); ok {
			return r.decisionForTarget(routed, req)
		}
	}
	targetNodeID, ok := r.targetForRequest(method)
	if !ok || targetNodeID == "" || strings.EqualFold(targetNodeID, r.localNodeID) {
		return distributedForwardDecision{}, false, nil
	}
	if sessionKey != "" {
		r.routeStore.set(sessionKey, targetNodeID)
	}
	return r.decisionForTarget(targetNodeID, req)
}

func distributedForwardableMethod(method string) bool {
	return method == "session.start" || method == "session.message"
}

func distributedSessionRouteKey(request shared.RPCRequest) string {
	params := shared.AsMap(request.Params)
	if params == nil {
		return ""
	}
	if sessionID := strings.TrimSpace(shared.StringArg(params, "sessionId", "")); sessionID != "" {
		return "session:" + sessionID
	}
	if threadID := strings.TrimSpace(shared.StringArg(params, "threadId", "")); threadID != "" {
		return "thread:" + threadID
	}
	return ""
}

func (r *distributedTaskRouter) targetForRequest(method string) (string, bool) {
	for _, rule := range r.rules {
		if !distributedRuleMatchesMethod(rule, method) {
			continue
		}
		if target := strings.TrimSpace(rule.Target.NodeID); target != "" {
			return target, true
		}
		target, ok := r.selectNode(rule.Target)
		return target, ok
	}
	return "", false
}

func distributedRuleMatchesMethod(rule DistributedForwardRuleConfig, method string) bool {
	for _, candidate := range rule.Methods {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || candidate == method {
			return true
		}
	}
	return false
}

func (r *distributedTaskRouter) selectNode(target DistributedForwardTargetConfig) (string, bool) {
	candidates := make([]string, 0, len(r.nodes))
	for id, node := range r.nodes {
		if id == r.localNodeID {
			continue
		}
		if !distributedNodeMatchesSelector(node, target.Selector) {
			continue
		}
		candidates = append(candidates, id)
	}
	if len(candidates) == 0 {
		return "", false
	}
	sort.Strings(candidates)
	strategy := strings.TrimSpace(target.Strategy)
	if strategy == "" || strings.EqualFold(strategy, "first") {
		return candidates[0], true
	}
	if strings.EqualFold(strategy, "round_robin") {
		key := distributedSelectorKey(target.Selector)
		r.mu.Lock()
		index := r.roundRobin[key] % len(candidates)
		r.roundRobin[key]++
		r.mu.Unlock()
		return candidates[index], true
	}
	return candidates[0], true
}

func distributedNodeMatchesSelector(node DistributedNodeConfig, selector DistributedForwardSelectorConfig) bool {
	if role := strings.TrimSpace(selector.Role); role != "" && !strings.EqualFold(strings.TrimSpace(node.Role), role) {
		return false
	}
	if zone := strings.TrimSpace(selector.Zone); zone != "" && !strings.EqualFold(strings.TrimSpace(node.Zone), zone) {
		return false
	}
	if capability := strings.TrimSpace(selector.Capability); capability != "" && !distributedNodeHasCapability(node, capability) {
		return false
	}
	return true
}

func distributedNodeHasCapability(node DistributedNodeConfig, capability string) bool {
	for _, candidate := range node.Capabilities {
		if strings.EqualFold(strings.TrimSpace(candidate), capability) {
			return true
		}
	}
	return false
}

func distributedSelectorKey(selector DistributedForwardSelectorConfig) string {
	return strings.Join([]string{
		strings.TrimSpace(selector.Role),
		strings.TrimSpace(selector.Zone),
		strings.TrimSpace(selector.Capability),
	}, "|")
}

func (r *distributedTaskRouter) decisionForTarget(targetNodeID string, req *http.Request) (distributedForwardDecision, bool, error) {
	targetNodeID = strings.TrimSpace(targetNodeID)
	nextHopID := r.nextHopForTarget(targetNodeID)
	if nextHopID == "" || strings.EqualFold(nextHopID, r.localNodeID) {
		return distributedForwardDecision{}, false, nil
	}
	node, ok := r.nodes[nextHopID]
	if !ok {
		return distributedForwardDecision{}, false, fmt.Errorf("TASK_FORWARD_PEER_UNKNOWN: %s", nextHopID)
	}
	if err := r.validateHopLimit(req); err != nil {
		return distributedForwardDecision{}, false, err
	}
	endpoint := strings.TrimSpace(node.BridgeEndpoint)
	if endpoint == "" {
		return distributedForwardDecision{}, false, fmt.Errorf("TASK_FORWARD_ENDPOINT_MISSING: %s", nextHopID)
	}
	return distributedForwardDecision{
		targetNodeID: targetNodeID,
		nextHopID:    nextHopID,
		endpoint:     endpoint,
	}, true, nil
}

func (r *distributedTaskRouter) nextHopForTarget(targetNodeID string) string {
	if nextHopID := strings.TrimSpace(r.routes[targetNodeID]); nextHopID != "" {
		return nextHopID
	}
	return targetNodeID
}

func (r *distributedTaskRouter) validateHopLimit(req *http.Request) error {
	nextHop := distributedForwardHop(req) + 1
	if nextHop > r.hopLimit {
		return fmt.Errorf("TASK_FORWARD_HOP_LIMIT_EXCEEDED: %d", r.hopLimit)
	}
	return nil
}

func (r *distributedTaskRouter) copyForwardRequestHeaders(dst http.Header, src http.Header, request shared.RPCRequest, decision distributedForwardDecision) {
	dst.Set("Content-Type", "application/json")
	dst.Set(distributedForwardedHeader, "1")
	dst.Set(distributedSourceHeader, firstForwardHeader(src, distributedSourceHeader, r.localNodeID))
	dst.Set(distributedTargetHeader, decision.targetNodeID)
	dst.Set(distributedTraceHeader, firstForwardHeader(src, distributedTraceHeader, distributedTraceID(request)))
	dst.Set(distributedHopHeader, strconv.Itoa(distributedForwardHopFromHeader(src.Get(distributedHopHeader))+1))
	copyForwardHeader(dst, src, "Accept")
	copyForwardHeader(dst, src, "Origin")
	if r.token != "" {
		dst.Set("Authorization", distributedForwardBearerHeader(r.token))
	}
}

func distributedTraceID(request shared.RPCRequest) string {
	if request.ID != nil {
		return fmt.Sprint(request.ID)
	}
	return strconv.FormatInt(time.Now().UnixNano(), 36)
}

func firstForwardHeader(src http.Header, key string, fallback string) string {
	if value := strings.TrimSpace(src.Get(key)); value != "" {
		return value
	}
	return fallback
}

func distributedForwardHop(req *http.Request) int {
	if req == nil {
		return 0
	}
	return distributedForwardHopFromHeader(req.Header.Get(distributedHopHeader))
}

func distributedForwardHopFromHeader(value string) int {
	hop, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || hop < 0 {
		return 0
	}
	return hop
}

func distributedForwardURL(endpoint string, path string) (string, error) {
	endpoint = strings.TrimRight(strings.TrimSpace(endpoint), "/")
	base, err := url.Parse(endpoint)
	if err != nil || base.Scheme == "" || base.Host == "" {
		return "", fmt.Errorf("TASK_FORWARD_ENDPOINT_INVALID: %s", endpoint)
	}
	if !distributedForwardEndpointPrivate(base) {
		return "", fmt.Errorf("TASK_FORWARD_ENDPOINT_INSECURE: use a private VPN endpoint")
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/" + strings.TrimLeft(path, "/")
	base.RawQuery = ""
	return base.String(), nil
}

func distributedForwardEndpointPrivate(endpoint *url.URL) bool {
	if endpoint == nil {
		return false
	}
	if !strings.EqualFold(endpoint.Scheme, "http") {
		return false
	}
	host := strings.Trim(endpoint.Hostname(), "[]")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
}

func distributedForwardBearerHeader(token string) string {
	token = strings.TrimSpace(token)
	if token == "" || strings.HasPrefix(strings.ToLower(token), "bearer ") {
		return token
	}
	return "Bearer " + token
}

func copyForwardHeader(dst http.Header, src http.Header, key string) {
	if value := strings.TrimSpace(src.Get(key)); value != "" {
		dst.Set(key, value)
	}
}

func copyForwardResponseHeaders(dst http.Header, src http.Header) {
	for _, key := range []string{"Content-Type", "Cache-Control", "Connection"} {
		if value := strings.TrimSpace(src.Get(key)); value != "" {
			dst.Set(key, value)
		}
	}
}

func newDistributedSessionRouteStore(ttl time.Duration) *distributedSessionRouteStore {
	return &distributedSessionRouteStore{
		routes: make(map[string]distributedSessionRoute),
		ttl:    ttl,
	}
}

func (s *distributedSessionRouteStore) get(key string) (string, bool) {
	if s == nil || strings.TrimSpace(key) == "" {
		return "", false
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	route, ok := s.routes[key]
	if !ok {
		return "", false
	}
	if !route.ExpiresAt.IsZero() && now.After(route.ExpiresAt) {
		delete(s.routes, key)
		return "", false
	}
	return route.TargetNodeID, true
}

func (s *distributedSessionRouteStore) set(key string, targetNodeID string) {
	if s == nil || strings.TrimSpace(key) == "" || strings.TrimSpace(targetNodeID) == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.routes[key] = distributedSessionRoute{
		TargetNodeID: strings.TrimSpace(targetNodeID),
		ExpiresAt:    time.Now().Add(s.ttl),
	}
}
