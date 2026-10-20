package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"kiro-proxy/config"
	refcache "kiro-proxy/internal/sub2apiref/cache"

	"github.com/google/uuid"
)

const (
	//! Claude Code's WebSearch tool sends this prefix in a sub-request whose only tool is native web_search.
	claudeCodeWebSearchPrefix = "Perform a web search for the query:"
	maxClaudeWebSearchUses    = 5
	webSearchTextChunkRunes   = 100
)

var (
	errNoWebSearchAccounts = errors.New("no available accounts for web_search")
	errNoClaudeAccounts    = errors.New("no available accounts")
)

type claudeWebSearchMode int

const (
	claudeWebSearchNone claudeWebSearchMode = iota
	claudeWebSearchDirect
	claudeWebSearchLoop
)

type claudeWebSearchRound struct {
	cacheUsage         promptCacheUsage
	commitCache        func()
	text               string
	thinking           string
	thinkingBlocks     []ClaudeContentBlock
	toolUses           []KiroToolUse
	inputTokens        int
	outputTokens       int
	nativeInputTokens  int
	nativeOutputTokens int
	credits            float64
	stopReason         string
	streamDiag         streamDiagnostic
}

type exactHostedSearchMeterContextKey struct{}
type exactHostedSearchStateContextKey struct{}
type directWebSearchCacheContextKey struct{}

func withExactHostedSearchMeter(ctx context.Context, meter *exactRequestMeter) context.Context {
	if meter == nil {
		return ctx
	}
	return context.WithValue(ctx, exactHostedSearchMeterContextKey{}, meter)
}

// The dedicated reference search path holds the original estimate and cache
// plan across all iterations. JSON uses final usage; dedicated SSE suppresses
// every inner message usage event and exposes only the original start usage.
type exactHostedSearchState struct {
	meter      *exactRequestMeter
	finalMeter *exactRequestMeter
	direct     bool
	profile    *promptCacheProfile
	accounts   map[string]*exactHostedSearchAccount
	startUsage promptCacheUsage
}

type exactHostedSearchAccount struct {
	usage  promptCacheUsage
	commit func()
}

func newExactHostedSearchState(meter *exactRequestMeter) *exactHostedSearchState {
	if meter == nil {
		return nil
	}
	return &exactHostedSearchState{meter: meter,
		profile:  buildExactCacheProfile(meter.Protocol, meter.Body, meter.Model, meter.EstimatedInput),
		accounts: make(map[string]*exactHostedSearchAccount)}
}

func (s *exactHostedSearchState) prepare(payload *KiroPayload, tracker *promptCacheTracker, account string, settings config.PromptCacheSettings) promptCacheUsage {
	meter := *s.meter
	meter.HostedSearch = true
	meter.Result = nil
	payload.exactMeter = &meter
	payload.outputBudget = nil
	entry := s.accounts[account]
	if entry == nil {
		usage := tracker.ComputeWithSettings(account, s.profile, settings)
		bindExactCache(payload, usage)
		entry = &exactHostedSearchAccount{usage: usage, commit: meter.Commit}
		s.accounts[account] = entry
	} else {
		bindExactCache(payload, entry.usage)
	}
	// The original bind's once is shared across every round and retry.
	meter.Commit = entry.commit
	s.startUsage = entry.usage
	return entry.usage
}

func exactHostedRoundUsage(payload *KiroPayload, initial promptCacheUsage) (int, int, promptCacheUsage, bool) {
	if payload != nil && exactDedicatedHostedStream(payload.exactMeter) {
		usage := initial
		ordinary := payload.exactMeter.EstimatedInput
		if initial.exactPlan != nil && initial.exactPlan.Usage() != nil {
			ordinary = initial.exactPlan.Usage().InputTokens
		} else if initial.exactInput != nil {
			ordinary = *initial.exactInput
		}
		usage.exactInput = &ordinary
		// Outer message_start has no nested TTL fields.
		usage.CacheCreation5mInputTokens, usage.CacheCreation1hInputTokens = 0, 0
		return ordinary + usage.CacheReadInputTokens + usage.CacheCreationInputTokens, 0, usage, true
	}
	input, output, usage, ok := exactClaudeUsage(payload, initial)
	if !ok {
		return input, output, usage, false
	}
	if payload.exactMeter.Protocol != "anthropic" && payload.exactMeter.ConvertedUsage != nil {
		v := payload.exactMeter.ConvertedUsage
		usage.CacheCreationInputTokens = v.CacheCreationInputTokens
		usage.CacheReadInputTokens = 0
		if v.InputTokensDetails != nil {
			usage.CacheReadInputTokens = v.InputTokensDetails.CachedTokens
		}
		ordinary := v.InputTokens - usage.CacheReadInputTokens - usage.CacheCreationInputTokens
		usage.exactInput = &ordinary
		input, output = v.InputTokens, v.OutputTokens
	}
	if payload.exactMeter.Stream || payload.exactMeter.Protocol != "anthropic" {
		// The reference stream parser receives an empty context, so its final
		// delta has ordinary input only. The surrounding Anthropic/OpenAI
		// collector retains initial message_start cache fields when absent.
		if usage.CacheReadInputTokens == 0 {
			usage.CacheReadInputTokens = initial.CacheReadInputTokens
		}
		if usage.CacheCreationInputTokens == 0 {
			usage.CacheCreationInputTokens = initial.CacheCreationInputTokens
			usage.CacheCreation5mInputTokens = initial.CacheCreation5mInputTokens
			usage.CacheCreation1hInputTokens = initial.CacheCreation1hInputTokens
		}
		input = *usage.exactInput + usage.CacheReadInputTokens + usage.CacheCreationInputTokens
	}
	return input, output, usage, true
}

func exactDedicatedHostedStream(meter *exactRequestMeter) bool {
	if meter == nil || (!meter.Stream && meter.Protocol == "anthropic") {
		return false
	}
	body, err := refcache.AnthropicBodyForModel(meter.Protocol, meter.Body, meter.Model)
	if err != nil {
		return false
	}
	var request struct {
		Tools []struct {
			Type string `json:"type"`
			Name string `json:"name"`
		} `json:"tools"`
	}
	if json.Unmarshal(body, &request) != nil || len(request.Tools) != 1 {
		return false
	}
	tool := request.Tools[0]
	return strings.HasPrefix(tool.Type, "web_search") || tool.Type == "google_search" ||
		tool.Name == "web_search" || tool.Name == "google_search" || tool.Name == "web_search_20250305"
}

type webSearchOutcome struct {
	query   string
	results []WebSearchResult
	//! errorCode uses Anthropic's web_search_tool_result_error codes, e.g. "unavailable".
	errorCode string
	errorText string
}

func isNativeWebSearchTool(tool ClaudeTool) bool {
	//! Only the typed server tool counts; a client tool that is merely named web_search stays a client tool.
	return strings.TrimSpace(tool.Name) == webSearchToolName &&
		strings.HasPrefix(strings.ToLower(strings.TrimSpace(tool.Type)), "web_search_")
}

func hasNativeWebSearchTool(tools []ClaudeTool) bool {
	for _, tool := range tools {
		if isNativeWebSearchTool(tool) {
			return true
		}
	}
	return false
}

func clientToolShadowsWebSearch(tools []ClaudeTool) bool {
	//! A client tool with the same Kiro name wins; the model then calls that tool and the client runs it.
	kiroName := shortenToolName(sanitizeToolName(webSearchToolName))
	for _, tool := range tools {
		if !isNativeWebSearchTool(tool) && shortenToolName(sanitizeToolName(tool.Name)) == kiroName {
			return true
		}
	}
	return false
}

func withKiroWebSearchTool(tools []ClaudeTool) []ClaudeTool {
	if !hasNativeWebSearchTool(tools) {
		return tools
	}
	out := make([]ClaudeTool, 0, len(tools))
	for _, tool := range tools {
		if !isNativeWebSearchTool(tool) {
			out = append(out, tool)
		}
	}
	if clientToolShadowsWebSearch(tools) {
		return out
	}
	return append(out, ClaudeTool{
		Name:        webSearchToolName,
		Description: "Search the web for current information.",
		InputSchema: webSearchInputSchema(),
	})
}

func claudeWebSearchModeFor(req *ClaudeRequest) (claudeWebSearchMode, string) {
	if req == nil || !hasNativeWebSearchTool(req.Tools) || clientToolShadowsWebSearch(req.Tools) {
		return claudeWebSearchNone, ""
	}
	choice, name := claudeToolChoice(req.ToolChoice)
	if choice == "none" || (choice == "tool" && name != webSearchToolName) {
		return claudeWebSearchNone, ""
	}
	//! Only Claude Code's search sub-request skips the model; a chat that merely allows search still gets an answer.
	if len(req.Tools) == 1 && req.structuredOutput == nil {
		if query := claudeCodeSearchQuery(req.Messages); query != "" {
			return claudeWebSearchDirect, query
		}
	}
	return claudeWebSearchLoop, ""
}

func claudeCodeSearchQuery(messages []ClaudeMessage) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if strings.TrimSpace(messages[i].Role) != "user" {
			continue
		}
		for _, text := range claudeTextBlocks(messages[i].Content) {
			trimmed := strings.TrimSpace(text)
			if strings.HasPrefix(trimmed, claudeCodeWebSearchPrefix) {
				return strings.TrimSpace(strings.TrimPrefix(trimmed, claudeCodeWebSearchPrefix))
			}
		}
		return ""
	}
	return ""
}

func claudeTextBlocks(content interface{}) []string {
	switch c := content.(type) {
	case string:
		return []string{c}
	case []interface{}:
		texts := make([]string, 0, len(c))
		for _, raw := range c {
			block, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			if blockType, _ := block["type"].(string); blockType != "text" {
				continue
			}
			if text, ok := block["text"].(string); ok {
				texts = append(texts, text)
			}
		}
		return texts
	}
	return nil
}

func resolveClaudeWebSearchMaxUses(tools []ClaudeTool) int {
	//! Client max_uses may lower the budget, never raise it past the proxy cap.
	maxUses := 0
	for _, tool := range tools {
		if isNativeWebSearchTool(tool) && tool.MaxUses > maxUses {
			maxUses = tool.MaxUses
		}
	}
	if maxUses <= 0 || maxUses > maxClaudeWebSearchUses {
		return maxClaudeWebSearchUses
	}
	return maxUses
}

func newServerToolUseID() string {
	return "srvtoolu_" + strings.ReplaceAll(uuid.NewString(), "-", "")
}

func webSearchPageAge(published string) interface{} {
	published = strings.TrimSpace(published)
	if published == "" {
		return nil
	}
	//! MCP sends epoch milliseconds; Anthropic shows page_age as a readable date.
	if ms, err := strconv.ParseInt(published, 10, 64); err == nil && ms > 0 {
		return time.UnixMilli(ms).UTC().Format("January 2, 2006")
	}
	return published
}

func webSearchResultBlocks(results []WebSearchResult) []map[string]interface{} {
	blocks := make([]map[string]interface{}, 0, len(results))
	for _, result := range results {
		blocks = append(blocks, map[string]interface{}{
			"type":              "web_search_result",
			"title":             result.Title,
			"url":               result.URL,
			"encrypted_content": result.Snippet,
			"page_age":          webSearchPageAge(result.PublishedDate),
		})
	}
	return blocks
}

func webSearchPresentationBlocks(outcome webSearchOutcome) []map[string]interface{} {
	id := newServerToolUseID()
	var resultContent interface{} = webSearchResultBlocks(outcome.results)
	if outcome.errorCode != "" {
		resultContent = map[string]interface{}{
			"type":       "web_search_tool_result_error",
			"error_code": outcome.errorCode,
		}
	}
	return []map[string]interface{}{
		{
			"type":  "server_tool_use",
			"id":    id,
			"name":  webSearchToolName,
			"input": map[string]interface{}{"query": outcome.query},
		},
		{
			"type":        "web_search_tool_result",
			"tool_use_id": id,
			"content":     resultContent,
		},
	}
}

func generateWebSearchSummary(query string, results []WebSearchResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Here are the search results for %q:\n\n", query)
	if len(results) == 0 {
		b.WriteString("No results found.\n")
	}
	for i, result := range results {
		fmt.Fprintf(&b, "%d. **%s**\n", i+1, strings.TrimSpace(result.Title))
		if snippet := []rune(strings.TrimSpace(result.Snippet)); len(snippet) > 0 {
			if len(snippet) > 200 {
				snippet = append(snippet[:200], []rune("...")...)
			}
			fmt.Fprintf(&b, "   %s\n", string(snippet))
		}
		fmt.Fprintf(&b, "   Source: %s\n\n", result.URL)
	}
	b.WriteString("\nPlease note that these are web search results and may not be fully accurate or up-to-date.")
	return b.String()
}

// Exact buildTextSummary from reference gateway_websearch_emulation.go; only
// the local search-result type and function name differ.
func exactWebSearchSummary(query string, results []WebSearchResult) string {
	if len(results) == 0 {
		return "No search results found for: " + query
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Here are the search results for \"%s\":\n\n", query)
	for i, r := range results {
		fmt.Fprintf(&sb, "%d. **%s**\n   %s\n   %s\n\n", i+1, r.Title, r.URL, r.Snippet)
	}
	return sb.String()
}

func webSearchOutcomeToolResultText(outcome webSearchOutcome) string {
	if outcome.errorCode != "" {
		return "web_search failed (" + outcome.errorCode + "): " + outcome.errorText
	}
	return generateWebSearchSummary(outcome.query, outcome.results)
}

func webSearchResultHistoryText(content interface{}) string {
	items, ok := content.([]interface{})
	if !ok || len(items) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\nWeb search results:")
	for _, raw := range items {
		item, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		title := strings.TrimSpace(scalarString(item["title"]))
		url := strings.TrimSpace(scalarString(item["url"]))
		if title == "" && url == "" {
			continue
		}
		fmt.Fprintf(&b, "\n- %s (%s)", title, url)
	}
	return b.String()
}

func splitWebSearchToolUses(toolUses []KiroToolUse) (search, client []KiroToolUse) {
	for _, tu := range toolUses {
		//! Exact name only: Claude Code's own client tool "WebSearch" must reach the client.
		if tu.Name == webSearchToolName {
			search = append(search, tu)
		} else {
			client = append(client, tu)
		}
	}
	return search, client
}

func (h *Handler) searchWebWithFailover(ctx context.Context, model, query string) ([]WebSearchResult, *config.Account, error) {
	excluded := make(map[string]bool)
	var lastErr error
	var lastAccount *config.Account
	for attempt := 0; attempt < newRequestRetryPlan().maxPerRequest; attempt++ {
		account := h.pool.GetNextForModelForAPIKeyExcluding(apiKeyIDFromContext(ctx), model, excluded)
		if account == nil {
			break
		}
		excluded[account.ID] = true
		lastAccount = account
		if err := h.ensureValidTokenContext(ctx, account); err != nil {
			if ctx.Err() != nil {
				return nil, account, ctx.Err()
			}
			lastErr = err
			h.handleAccountFailure(account, err)
			recordAttemptError(account, model, 0, err)
			continue
		}
		if prepare, ok := ctx.Value(directWebSearchCacheContextKey{}).(func(*config.Account)); ok {
			prepare(account)
		}
		results, err := performKiroWebSearch(ctx, account, query)
		if err == nil {
			return results, account, nil
		}
		if ctx.Err() != nil {
			return nil, account, ctx.Err()
		}
		//! MCP errors never touch account health: its 403 would otherwise ban an account that chats fine.
		lastErr = err
		recordAttemptError(account, model, 0, err)
	}
	if lastErr == nil {
		lastErr = errNoWebSearchAccounts
	}
	return nil, lastAccount, lastErr
}

func (h *Handler) runWebSearches(ctx context.Context, model string, toolUses []KiroToolUse, used *int, maxUses int) []webSearchOutcome {
	outcomes := make([]webSearchOutcome, 0, len(toolUses))
	for _, tu := range toolUses {
		outcome := webSearchOutcome{query: webSearchQueryFromInput(tu.Input)}
		switch {
		case outcome.query == "":
			outcome.errorCode, outcome.errorText = "invalid_input", "query is required"
		case *used >= maxUses:
			outcome.errorCode, outcome.errorText = "max_uses_exceeded", fmt.Sprintf("the web_search limit of %d uses is reached; answer with the results you have", maxUses)
		default:
			*used++
			results, _, err := h.searchWebWithFailover(ctx, model, outcome.query)
			if err != nil {
				outcome.errorCode, outcome.errorText = "unavailable", err.Error()
			}
			outcome.results = results
		}
		outcomes = append(outcomes, outcome)
	}
	return outcomes
}

func (h *Handler) handleClaudeDirectWebSearch(w http.ResponseWriter, r *http.Request, model, responseModel, query string, estimatedInputTokens int, apiKeyReservation *apiKeyUsageReservation, stream bool, maxTokens int) {
	defer apiKeyReservation.release()
	if responseModel == "" {
		responseModel = model
	}
	settings := config.EffectivePromptCacheSettings(apiKeyReservation.apiKeyID())
	meter, _ := r.Context().Value(exactHostedSearchMeterContextKey{}).(*exactRequestMeter)
	cacheUsage := promptCacheUsage{}
	if meter != nil {
		estimatedInputTokens = meter.EstimatedInput
		profile := buildExactCacheProfile(meter.Protocol, meter.Body, meter.Model, meter.EstimatedInput)
		// The generic direct-search reference commits before invoking its search
		// provider, including when that provider subsequently fails.
		r = r.WithContext(context.WithValue(r.Context(), directWebSearchCacheContextKey{}, func(account *config.Account) {
			cacheUsage = h.promptCache.ComputeWithSettings(account.ID, profile, settings)
			if cacheUsage.exactPlan != nil {
				cacheUsage.exactPlan.Commit()
				if u := cacheUsage.exactPlan.Usage(); u != nil {
					ordinary := u.InputTokens
					cacheUsage.exactInput = &ordinary
				}
			}
		}))
	}

	results, account, err := h.searchWebWithFailover(r.Context(), model, query)
	if err != nil {
		if isContextCanceledError(err) {
			recordClientDisconnect(r.Context(), apiKeyReservation, account, model)
			return
		}
		status := http.StatusBadGateway
		if errors.Is(err, errNoWebSearchAccounts) {
			status = http.StatusServiceUnavailable
		}
		recordFinalRequestForApiKey(r.Context(), apiKeyReservation, account, model, 0, 0, 0, false, status, err.Error())
		h.sendClaudeError(w, status, "api_error", "Web search failed: "+err.Error())
		return
	}

	content := []map[string]interface{}{{"type": "text", "text": fmt.Sprintf("I'll search for %q.", query)}}
	content = append(content, webSearchPresentationBlocks(webSearchOutcome{query: query, results: results})...)
	content = append(content, map[string]interface{}{"type": "text", "text": generateWebSearchSummary(query, results)})
	if meter != nil {
		content = append(webSearchPresentationBlocks(webSearchOutcome{query: query, results: results}), map[string]interface{}{"type": "text", "text": exactWebSearchSummary(query, results)})
	}
	budget := newOutputBudget(maxTokens)
	if meter != nil {
		budget = nil
	}
	if budget != nil {
		limited := make([]map[string]interface{}, 0, len(content))
		admittedTools := make(map[string]bool)
		for _, block := range content {
			switch block["type"] {
			case "text":
				text, _ := block["text"].(string)
				if text = budget.text(text, false); text != "" {
					limited = append(limited, map[string]interface{}{"type": "text", "text": text})
				}
			case "server_tool_use":
				input, _ := block["input"].(map[string]interface{})
				if budget.tool(KiroToolUse{Name: scalarString(block["name"]), Input: input}) {
					admittedTools[scalarString(block["id"])] = true
					limited = append(limited, block)
				}
			case "web_search_tool_result":
				if admittedTools[scalarString(block["tool_use_id"])] {
					limited = append(limited, block)
				}
			}
		}
		content = limited
	}

	inputTokens := estimatedInputTokens
	outputTokens := estimateWebSearchContentTokens(content)
	if meter != nil {
		outputTokens = len(exactWebSearchSummary(query, results)) / 4
	}
	stopReason := "end_turn"
	if budget != nil {
		if budget.stopped {
			stopReason = "max_tokens"
		}
	}
	updateRequestUsageDiagnostic(r.Context(), func(diagnostic *usageDiagnostic) {
		diagnostic.CacheUsageSource = "not_applicable"
		if meter != nil {
			diagnostic.CacheUsageSource = "sub2api_reference"
			diagnostic.CacheReadInputTokens = cacheUsage.CacheReadInputTokens
			diagnostic.CacheCreationInputTokens = cacheUsage.CacheCreationInputTokens
		}
		diagnostic.NativeInputTokens = 0
		diagnostic.NativeOutputTokens = 0
		diagnostic.ReportedInputTokens = inputTokens
		diagnostic.ReportedOutputTokens = outputTokens
		diagnostic.InputTokenSource = "local_estimate"
		diagnostic.OutputTokenSource = "local_estimate"
		diagnostic.EstimatedReturnedOutputTokens = outputTokens
		diagnostic.ReturnedOutputBytes = claudeBlocksByteLength(content)
		diagnostic.ResponseStopReason = stopReason
		if budget != nil {
			diagnostic.LocalOutputBudgetLimit = budget.limit
			diagnostic.LocalOutputBudgetUsed = budget.used
			diagnostic.LocalOutputBudgetStopped = budget.stopped
			diagnostic.LocalOutputBudgetTruncated = budget.truncated
		}
	})
	h.recordSuccessForApiKey(apiKeyReservation, inputTokens, outputTokens, 0)
	getObserveStore().RecordSuccess(account.ID, model, inputTokens, outputTokens, 0)
	recordFinalRequestForApiKey(r.Context(), apiKeyReservation, account, model, inputTokens, outputTokens, 0, true, 200, "")

	var exactState *exactHostedSearchState
	if meter != nil {
		exactState = &exactHostedSearchState{meter: meter, direct: true, startUsage: cacheUsage}
	}
	h.writeClaudeWebSearchResponse(w, stream, responseModel, content, stopReason, inputTokens, outputTokens, 1, cacheUsage, settings.Enabled, exactState)
}

func (h *Handler) runClaudeWebSearchLoop(w http.ResponseWriter, r *http.Request, req *ClaudeRequest, thinking bool, thinkingOpts claudeThinkingResponseOptions, estimatedInputTokens int, apiKeyReservation *apiKeyUsageReservation) {
	defer apiKeyReservation.release()
	ctx := r.Context()
	cacheSettings := config.EffectivePromptCacheSettings(apiKeyReservation.apiKeyID())
	meter, _ := ctx.Value(exactHostedSearchMeterContextKey{}).(*exactRequestMeter)
	if meter != nil {
		copy := *meter
		copy.Stream = req.Stream
		meter = &copy
	}
	if meter == nil && cacheSettings.Mode == config.PromptCacheModeSub2API {
		body, err := json.Marshal(req)
		if err != nil {
			h.sendClaudeError(w, 400, "invalid_request_error", err.Error())
			return
		}
		meter, err = newExactRequestMeter(ctx, cacheSettings, "anthropic", body, req.Model, req.Stream, thinking, req.MaxTokens)
		if err != nil {
			h.sendClaudeError(w, 400, "invalid_request_error", err.Error())
			return
		}
	}
	exactSearch := newExactHostedSearchState(meter)
	if exactSearch != nil {
		ctx = context.WithValue(ctx, exactHostedSearchStateContextKey{}, exactSearch)
		estimatedInputTokens = meter.EstimatedInput
	}
	budget := newOutputBudget(req.MaxTokens)
	if exactSearch != nil {
		budget = nil
	}
	if budget != nil {
		ctx = context.WithValue(ctx, outputBudgetContextKey{}, budget)
	}

	working := *req
	working.Messages = append([]ClaudeMessage(nil), req.Messages...)
	maxUses := resolveClaudeWebSearchMaxUses(req.Tools)
	presented := make([]map[string]interface{}, 0)
	searches := 0
	outputTokens := 0
	inputTokens := 0
	var credits float64
	var lastAccount *config.Account
	cacheUsage := promptCacheUsage{}
	var pendingCacheCommits []func()

	//! Every search round spends budget, so at most maxUses+2 upstream calls run before the final answer.
	for roundIdx := 0; ; roundIdx++ {
		if roundIdx > 0 && exactSearch == nil {
			estimatedInputTokens = estimateClaudeInputWithSettings(&working, cacheSettings)
		}

		round, account, err := h.callClaudeRoundWithFailover(ctx, &working, thinking, estimatedInputTokens, apiKeyReservation.apiKeyID())
		if account != nil {
			lastAccount = account
		}
		if err != nil {
			if isContextCanceledError(err) {
				recordClientDisconnect(r.Context(), apiKeyReservation, lastAccount, req.Model)
				return
			}
			status, message := http.StatusInternalServerError, err.Error()
			if errors.Is(err, errNoClaudeAccounts) {
				//! Same text as the plain Claude path, so clients see one message for an empty pool.
				status, message = http.StatusServiceUnavailable, "No available accounts"
			}
			recordFinalRequestForApiKey(r.Context(), apiKeyReservation, lastAccount, req.Model, 0, 0, 0, false, status, message)
			h.sendClaudeError(w, status, "api_error", message)
			return
		}
		if round.commitCache != nil {
			pendingCacheCommits = append(pendingCacheCommits, round.commitCache)
		}
		if exactSearch != nil {
			credits, outputTokens, inputTokens, cacheUsage = round.credits, round.outputTokens, round.inputTokens, round.cacheUsage
		} else {
			credits += round.credits
			outputTokens += round.outputTokens
			inputTokens += round.inputTokens
			cacheUsage.CacheCreationInputTokens += round.cacheUsage.CacheCreationInputTokens
			cacheUsage.CacheReadInputTokens += round.cacheUsage.CacheReadInputTokens
			cacheUsage.CacheCreation5mInputTokens += round.cacheUsage.CacheCreation5mInputTokens
			cacheUsage.CacheCreation1hInputTokens += round.cacheUsage.CacheCreation1hInputTokens
		}
		getObserveStore().RecordSuccess(account.ID, req.Model, round.inputTokens, round.outputTokens, round.credits)
		h.pool.RecordSuccess(account.ID)
		roundInputForStats := round.nativeInputTokens
		if roundInputForStats <= 0 {
			roundInputForStats = round.inputTokens
		}
		h.pool.UpdateStats(account.ID, roundInputForStats+round.outputTokens, round.credits)

		searchUses, clientUses := splitWebSearchToolUses(round.toolUses)
		if len(searchUses) > 0 && len(clientUses) == 0 && roundIdx <= maxUses && (budget == nil || !budget.stopped) {
			outcomes := h.runWebSearches(ctx, req.Model, searchUses, &searches, maxUses)
			if ctx.Err() != nil {
				recordClientDisconnect(r.Context(), apiKeyReservation, lastAccount, req.Model)
				return
			}
			appendWebSearchRound(&working, round, searchUses, outcomes)
			// The requested server tool has run; follow-up rounds may answer normally.
			working.ToolChoice = map[string]interface{}{"type": "auto"}
			for _, outcome := range outcomes {
				presented = append(presented, webSearchPresentationBlocks(outcome)...)
			}
			continue
		}

		//! Final round: searches next to client tools still run, but only the client sees their results.
		content := append([]map[string]interface{}(nil), presented...)
		finalText := round.text
		if req.structuredOutput != nil {
			finalText = normalizeClaudeStructuredJSONText(finalText)
		}
		if err := req.structuredOutput.validate(finalText, len(clientUses), mapClaudeStopReason(round.stopReason, len(clientUses))); err != nil {
			h.recordSuccessForApiKey(apiKeyReservation, inputTokens, outputTokens, credits)
			recordFinalRequestForApiKey(r.Context(), apiKeyReservation, lastAccount, req.Model, inputTokens, outputTokens, credits, false, 502, err.Error())
			h.sendClaudeError(w, 502, "api_error", err.Error())
			return
		}
		if thinking && (round.thinking != "" || len(round.thinkingBlocks) > 0) {
			switch {
			case thinkingOpts.OmitDisplay:
				if len(round.thinkingBlocks) > 0 {
					for _, block := range round.thinkingBlocks {
						block.Thinking = ""
						content = append(content, claudeThinkingBlockMap(block))
					}
				} else {
					content = append(content, map[string]interface{}{"type": "thinking", "thinking": ""})
				}
			case thinkingOpts.Format == "think":
				finalText = "<think>" + round.thinking + "</think>" + finalText
			case thinkingOpts.Format == "reasoning_content":
				finalText = round.thinking + finalText
			default:
				if len(round.thinkingBlocks) > 0 {
					for _, block := range round.thinkingBlocks {
						content = append(content, claudeThinkingBlockMap(block))
					}
				} else {
					content = append(content, map[string]interface{}{"type": "thinking", "thinking": round.thinking})
				}
			}
		}
		if strings.TrimSpace(finalText) != "" {
			content = append(content, map[string]interface{}{"type": "text", "text": finalText})
		}
		for _, tu := range round.toolUses {
			if tu.Name == webSearchToolName {
				if budget != nil && budget.stopped {
					continue
				}
				outcomes := h.runWebSearches(ctx, req.Model, []KiroToolUse{tu}, &searches, maxUses)
				content = append(content, webSearchPresentationBlocks(outcomes[0])...)
				continue
			}
			input := tu.Input
			if input == nil {
				input = map[string]interface{}{}
			}
			content = append(content, map[string]interface{}{
				"type":  "tool_use",
				"id":    tu.ToolUseID,
				"name":  tu.Name,
				"input": input,
			})
		}
		if ctx.Err() != nil {
			recordClientDisconnect(r.Context(), apiKeyReservation, lastAccount, req.Model)
			return
		}

		for _, commit := range pendingCacheCommits {
			commit()
		}
		stopReason := mapClaudeStopReason(round.stopReason, len(clientUses))
		updateRequestUsageDiagnostic(r.Context(), func(diagnostic *usageDiagnostic) {
			diagnostic.ReportedInputTokens = inputTokens
			diagnostic.ReportedOutputTokens = outputTokens
			diagnostic.CacheReadInputTokens = cacheUsage.CacheReadInputTokens
			diagnostic.CacheCreationInputTokens = cacheUsage.CacheCreationInputTokens
			diagnostic.EstimatedReturnedOutputTokens = estimateReturnedClaudeBlocks(content)
			visibleReasoningTokens := 0
			if thinking && !thinkingOpts.OmitDisplay {
				visibleReasoningTokens = estimateApproxTokens(round.thinking)
			}
			// Intermediate rounds and omitted final reasoning are not presented.
			diagnostic.SuppressedReasoningTokens = max(0, diagnostic.ObservedReasoningTokens-visibleReasoningTokens)
			diagnostic.ReturnedOutputBytes = claudeBlocksByteLength(content)
			diagnostic.ResponseStopReason = stopReason
			if budget != nil {
				diagnostic.LocalOutputBudgetLimit = budget.limit
				diagnostic.LocalOutputBudgetUsed = budget.used
				diagnostic.LocalOutputBudgetStopped = budget.stopped
				diagnostic.LocalOutputBudgetTruncated = budget.truncated
			}
		})
		h.recordSuccessForApiKey(apiKeyReservation, inputTokens, outputTokens, credits)
		recordFinalRequestForApiKey(r.Context(), apiKeyReservation, lastAccount, req.Model, inputTokens, outputTokens, credits, true, 200, "")

		responseModel := req.responseModel
		if responseModel == "" {
			responseModel = req.Model
		}
		h.writeClaudeWebSearchResponse(w, req.Stream, responseModel, content, stopReason, inputTokens, outputTokens, searches, cacheUsage, cacheSettings.Enabled, exactSearch)
		return
	}
}

func (h *Handler) callClaudeRoundWithFailover(ctx context.Context, req *ClaudeRequest, thinking bool, estimatedInputTokens int, apiKeyID string) (*claudeWebSearchRound, *config.Account, error) {
	payload := ClaudeToKiro(req, h.claudeNeedsThinkingPrompt(req, thinking))
	h.applyClaudeInferenceOptions(payload, req, thinking)
	cacheSettings := config.EffectivePromptCacheSettings(apiKeyID)
	cacheProfile := h.promptCache.BuildClaudeProfileWithSettings(cloneClaudeRequestForThinking(req, h.claudeNeedsThinkingPrompt(req, thinking)), estimatedInputTokens, cacheSettings)
	if cacheSettings.Mode == config.PromptCacheModeLifecycle {
		cacheProfile = buildLifecycleProfile(req)
		estimatedInputTokens = cacheProfile.TotalInputTokens
		payload.lifecycleRequest = req
	}
	exactSearch, _ := ctx.Value(exactHostedSearchStateContextKey{}).(*exactHostedSearchState)
	if exactSearch == nil && cacheSettings.Mode == config.PromptCacheModeSub2API {
		body, err := json.Marshal(req)
		if err != nil {
			return nil, nil, err
		}
		meter, err := newExactRequestMeter(ctx, cacheSettings, "anthropic", body, req.Model, req.Stream, thinking, req.MaxTokens)
		if err != nil {
			return nil, nil, err
		}
		exactSearch = newExactHostedSearchState(meter)
	}
	if exactSearch != nil {
		cacheProfile = exactSearch.profile
		estimatedInputTokens = exactSearch.meter.EstimatedInput
	}
	excluded := make(map[string]bool)
	var lastErr error
	var lastAccount *config.Account

	retryPlan := newRequestRetryPlan()
	totalAttempts := 0
	for totalAttempts < retryPlan.maxPerRequest && !isUpstreamClientError(lastErr) {
		account := h.pickAccountForAPIKey(apiKeyID, payload, req.Model, excluded)
		if account == nil {
			break
		}
		for accountAttempt := 0; accountAttempt < retryPlan.maxPerAccount && totalAttempts < retryPlan.maxPerRequest; accountAttempt++ {
			totalAttempts++
			var cacheUsage promptCacheUsage
			if exactSearch != nil {
				cacheUsage = exactSearch.prepare(payload, h.promptCache, account.ID, cacheSettings)
			} else {
				cacheUsage = h.promptCache.ComputeWithSettings(account.ID, cacheProfile, cacheSettings)
			}
			budget, _ := ctx.Value(outputBudgetContextKey{}).(*outputBudget)
			var before outputBudget
			if budget != nil {
				before = *budget
			}
			round, err := h.callClaudeRound(ctx, account, payload, req.Model, thinking, estimatedInputTokens)
			if err == nil {
				if exactSearch != nil {
					round.inputTokens, round.outputTokens, round.cacheUsage, _ = exactHostedRoundUsage(payload, cacheUsage)
					if exactDedicatedHostedStream(payload.exactMeter) {
						round.credits = 0
					}
					exactSearch.finalMeter = payload.exactMeter
				} else {
					round.cacheUsage = scalePromptCacheUsage(cacheUsage, cacheProfile, round.inputTokens)
				}
				if cacheSettings.Enabled && exactSearch == nil {
					profile := cacheProfileWithInputTokens(cacheProfile, round.inputTokens)
					if cacheSettings.Mode == config.PromptCacheModeLifecycle {
						// Later tool rounds or final schema validation can still fail.
						usage, accountID := round.cacheUsage, account.ID
						round.commitCache = func() { h.promptCache.UpdateWithSettings(accountID, profile, cacheSettings, usage) }
					} else {
						h.promptCache.UpdateWithSettings(account.ID, profile, cacheSettings, round.cacheUsage)
					}
				}
				h.rememberAccount(payload, account)
				return round, account, nil
			}
			// This path buffers a complete round. Failed attempts are discarded,
			// so their unreturned output must not consume the shared client cap.
			if budget != nil {
				*budget = before
			}
			lastErr = err
			lastAccount = account
			if isContextCanceledError(err) {
				return nil, account, err
			}
			h.handleAccountFailure(account, err)
			recordAttemptError(account, req.Model, 0, err)
			if retryPlan.canRetrySameAccount(err, accountAttempt, totalAttempts) {
				retryPlan.waitBeforeRetry(ctx, totalAttempts)
				continue
			}
			if retryPlan.shouldBackoffBeforeNextAccount(err, totalAttempts) {
				retryPlan.waitBeforeRetry(ctx, totalAttempts)
			}
			break
		}
		excluded[account.ID] = true
	}
	if lastErr == nil {
		lastErr = errNoClaudeAccounts
	}
	return nil, lastAccount, lastErr
}

func (h *Handler) callClaudeRound(ctx context.Context, account *config.Account, payload *KiroPayload, model string, thinking bool, estimatedInputTokens int) (*claudeWebSearchRound, error) {
	if err := h.ensureValidTokenContext(ctx, account); err != nil {
		return nil, err
	}
	var text, reasoning string
	var nativeThinking claudeThinkingCollector
	var toolUses []KiroToolUse
	var inputTokens, outputTokens int
	var credits float64
	var stopReason string
	var streamDiag streamDiagnostic
	callback := &KiroStreamCallback{
		OnThinkingSignature: nativeThinking.signature,
		OnText: func(t string, isThinking bool) {
			if isThinking {
				reasoning += t
				nativeThinking.text(t)
			} else {
				text += t
			}
		},
		OnToolUse:    func(tu KiroToolUse) { toolUses = append(toolUses, tu) },
		OnComplete:   func(inTok, outTok int) { inputTokens, outputTokens = inTok, outTok },
		OnCredits:    func(c float64) { credits = c },
		OnStopReason: func(reason string) { stopReason = reason },
		onStreamDiagnostic: func(diagnostic streamDiagnostic) {
			streamDiag = diagnostic
		},
	}
	if err := CallKiroAPIContext(ctx, account, payload, callback); err != nil {
		return nil, err
	}
	text, extracted := extractThinkingFromContent(text)
	usageReasoning := reasoning
	if reasoning == "" {
		reasoning = extracted
		usageReasoning = extracted
	}
	if !thinking {
		reasoning = ""
	}
	nativeInputTokens, nativeOutputTokens := inputTokens, outputTokens
	if payload.exactMeter == nil {
		providerInputTokens, providerOutputTokens := finalizeUsageTokens(inputTokens, outputTokens, estimatedInputTokens, estimateClaudeOutputTokens(text, usageReasoning, toolUses))
		inputTokens, outputTokens = publicClaudeUsageTokens(estimatedInputTokens, providerInputTokens, providerOutputTokens)
	} else if payload.exactMeter.Result != nil {
		stopReason = payload.exactMeter.Result.StopReason
	}
	updateRequestUsageDiagnostic(ctx, func(diagnostic *usageDiagnostic) {
		diagnostic.TranslatedInputEstimate += estimateKiroPayloadInputTokens(payload)
		diagnostic.KiroRounds++
		diagnostic.NativeInputTokens += nativeInputTokens
		diagnostic.NativeOutputTokens += nativeOutputTokens
		diagnostic.EstimatedGeneratedOutputTokens += estimateClaudeOutputTokens(text, usageReasoning, toolUses)
		diagnostic.ObservedReasoningTokens += estimateApproxTokens(usageReasoning)
		if !thinking {
			diagnostic.SuppressedReasoningTokens += estimateApproxTokens(usageReasoning)
		}
		diagnostic.InputTokenSource = "sub2api_reference"
		if payload.lifecycleRequest != nil {
			diagnostic.InputTokenSource = "prefix_lifecycle_local"
		}
		diagnostic.OutputTokenSource = usageTokenSource(diagnostic.NativeOutputTokens)
		diagnostic.ForwardedMaxOutputTokens = 0
		if payload.InferenceConfig != nil {
			diagnostic.ForwardedMaxOutputTokens = payload.InferenceConfig.MaxTokens
		}
		diagnostic.ObservedKiroStopReason = streamDiag.ObservedKiroStopReason
		diagnostic.LocalOutputBudgetLimit = streamDiag.OutputBudgetLimit
		diagnostic.LocalOutputBudgetUsed = streamDiag.OutputBudgetUsed
		diagnostic.LocalOutputBudgetStopped = streamDiag.OutputBudgetStopped
		diagnostic.LocalOutputBudgetTruncated = streamDiag.OutputBudgetTruncated
	})
	return &claudeWebSearchRound{
		text:               text,
		thinking:           reasoning,
		thinkingBlocks:     nativeThinking.responseBlocks(false),
		toolUses:           toolUses,
		inputTokens:        inputTokens,
		outputTokens:       outputTokens,
		nativeInputTokens:  nativeInputTokens,
		nativeOutputTokens: nativeOutputTokens,
		credits:            credits,
		stopReason:         stopReason,
		streamDiag:         streamDiag,
	}, nil
}

func appendWebSearchRound(req *ClaudeRequest, round *claudeWebSearchRound, toolUses []KiroToolUse, outcomes []webSearchOutcome) {
	//! []interface{} is the shape JSON decoding gives, so ClaudeToKiro reads these blocks like client history.
	assistant := make([]interface{}, 0, len(toolUses)+1)
	if strings.TrimSpace(round.text) != "" {
		assistant = append(assistant, map[string]interface{}{"type": "text", "text": round.text})
	}
	results := make([]interface{}, 0, len(toolUses))
	for i, tu := range toolUses {
		input := tu.Input
		if input == nil {
			input = map[string]interface{}{}
		}
		assistant = append(assistant, map[string]interface{}{
			"type":  "tool_use",
			"id":    tu.ToolUseID,
			"name":  tu.Name,
			"input": input,
		})
		results = append(results, map[string]interface{}{
			"type":        "tool_result",
			"tool_use_id": tu.ToolUseID,
			"content":     webSearchOutcomeToolResultText(outcomes[i]),
		})
	}
	req.Messages = append(req.Messages,
		ClaudeMessage{Role: "assistant", Content: assistant},
		ClaudeMessage{Role: "user", Content: results},
	)
}

// The reference's direct search adapter estimates its rendered summary by
// UTF-8 bytes / 4; server tool wrappers/results are not model generation.
func estimateWebSearchContentTokens(content []map[string]interface{}) int {
	bytes := 0
	for _, block := range content {
		if block["type"] == "text" {
			text, _ := block["text"].(string)
			bytes += len(text)
		}
	}
	return bytes / 4
}

func webSearchUsage(inputTokens, outputTokens, searches int, cacheUsage promptCacheUsage, includeCache bool) map[string]interface{} {
	usage := buildClaudeUsageMap(inputTokens, outputTokens, cacheUsage, includeCache)
	usage["server_tool_use"] = map[string]interface{}{"web_search_requests": searches}
	return usage
}

func (h *Handler) writeClaudeWebSearchResponse(w http.ResponseWriter, stream bool, model string, content []map[string]interface{}, stopReason string, inputTokens, outputTokens, searches int, cacheUsage promptCacheUsage, includeCache bool, exactSearchStates ...*exactHostedSearchState) {
	msgID := "msg_" + uuid.New().String()
	if !stream {
		responseUsage := webSearchUsage(inputTokens, outputTokens, searches, cacheUsage, includeCache)
		if len(exactSearchStates) > 0 && exactSearchStates[0] != nil {
			state := exactSearchStates[0]
			if state.direct {
				responseUsage = map[string]interface{}{"input_tokens": billedClaudeInputTokens(inputTokens, cacheUsage), "output_tokens": outputTokens}
				if cacheUsage.exactPlan != nil && cacheUsage.exactPlan.Usage() != nil {
					responseUsage["cache_creation_input_tokens"] = cacheUsage.CacheCreationInputTokens
					responseUsage["cache_read_input_tokens"] = cacheUsage.CacheReadInputTokens
				}
			} else if meter := state.finalMeter; meter != nil && meter.Result != nil {
				var reference struct {
					Usage map[string]interface{} `json:"usage"`
				}
				if json.Unmarshal(meter.Result.ResponseBody, &reference) == nil && reference.Usage != nil {
					responseUsage = reference.Usage
				}
			}
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id":            msgID,
			"type":          "message",
			"role":          "assistant",
			"model":         model,
			"content":       content,
			"stop_reason":   stopReason,
			"stop_sequence": nil,
			"usage":         responseUsage,
		})
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		h.sendClaudeError(w, http.StatusInternalServerError, "api_error", "Streaming not supported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	startUsage := webSearchUsage(inputTokens, 0, searches, cacheUsage, includeCache)
	deltaUsage := buildClaudeMessageDeltaUsage(outputTokens)
	if len(exactSearchStates) > 0 && exactSearchStates[0] != nil {
		state := exactSearchStates[0]
		startUsage = map[string]interface{}{"input_tokens": state.meter.EstimatedInput, "output_tokens": 0}
		if state.startUsage.exactPlan != nil {
			if initial := state.startUsage.exactPlan.Usage(); initial != nil {
				startUsage["input_tokens"] = initial.InputTokens
				startUsage["cache_creation_input_tokens"] = initial.CacheCreationInputTokens
				startUsage["cache_read_input_tokens"] = initial.CacheReadInputTokens
			}
		}
		if state.direct {
			startUsage["cache_creation_input_tokens"] = cacheUsage.CacheCreationInputTokens
			startUsage["cache_read_input_tokens"] = cacheUsage.CacheReadInputTokens
		} else {
			deltaUsage = map[string]interface{}{"input_tokens": billedClaudeInputTokens(inputTokens, cacheUsage), "output_tokens": outputTokens}
		}
		if meter := state.finalMeter; meter != nil && meter.Result != nil {
			u := meter.Result.Usage
			if u.KiroCredits > 0 {
				deltaUsage["_sub2api_kiro_credits"] = u.KiroCredits
			}
			if u.CacheReadInputTokens > 0 {
				deltaUsage["cache_read_input_tokens"] = u.CacheReadInputTokens
			}
			if u.CacheCreationInputTokens > 0 {
				deltaUsage["cache_creation_input_tokens"] = u.CacheCreationInputTokens
			}
			if u.CacheCreation5mInputTokens > 0 || u.CacheCreation1hInputTokens > 0 {
				deltaUsage["cache_creation"] = map[string]int{"ephemeral_5m_input_tokens": u.CacheCreation5mInputTokens, "ephemeral_1h_input_tokens": u.CacheCreation1hInputTokens}
			}
		}
		if !state.direct && exactDedicatedHostedStream(state.meter) {
			// Original AdjustSSEChunk suppresses all inner message usage, even
			// the final round. Keep valid closing frames as a transport extension.
			deltaUsage = map[string]interface{}{"output_tokens": 0}
		}
	}

	h.sendSSE(w, flusher, "message_start", map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id":            msgID,
			"type":          "message",
			"role":          "assistant",
			"model":         model,
			"content":       []interface{}{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         startUsage,
		},
	})
	for index, block := range content {
		h.sendWebSearchContentBlock(w, flusher, index, block)
	}
	h.sendSSE(w, flusher, "message_delta", map[string]interface{}{
		"type":  "message_delta",
		"delta": map[string]interface{}{"stop_reason": stopReason, "stop_sequence": nil},
		"usage": deltaUsage,
	})
	h.sendSSE(w, flusher, "message_stop", map[string]interface{}{"type": "message_stop"})
}

func (h *Handler) sendWebSearchContentBlock(w http.ResponseWriter, flusher http.Flusher, index int, block map[string]interface{}) {
	blockType, _ := block["type"].(string)
	start := block
	var deltas []map[string]interface{}
	switch blockType {
	case "thinking":
		start = map[string]interface{}{"type": "thinking", "thinking": ""}
		text, _ := block["thinking"].(string)
		for _, chunk := range chunkByRunes(text, webSearchTextChunkRunes) {
			deltas = append(deltas, map[string]interface{}{"type": "thinking_delta", "thinking": chunk})
		}
		if signature, _ := block["signature"].(string); signature != "" {
			deltas = append(deltas, map[string]interface{}{"type": "signature_delta", "signature": signature})
		}
	case "text":
		start = map[string]interface{}{"type": "text", "text": ""}
		text, _ := block["text"].(string)
		for _, chunk := range chunkByRunes(text, webSearchTextChunkRunes) {
			deltas = append(deltas, map[string]interface{}{"type": "text_delta", "text": chunk})
		}
	case "tool_use", "server_tool_use":
		//! Like the real API: empty input on start, then the whole input as one input_json_delta.
		start = make(map[string]interface{}, len(block))
		for k, v := range block {
			start[k] = v
		}
		start["input"] = map[string]interface{}{}
		partial, _ := json.Marshal(block["input"])
		deltas = append(deltas, map[string]interface{}{"type": "input_json_delta", "partial_json": string(partial)})
	}
	h.sendSSE(w, flusher, "content_block_start", map[string]interface{}{
		"type":          "content_block_start",
		"index":         index,
		"content_block": start,
	})
	for _, delta := range deltas {
		h.sendSSE(w, flusher, "content_block_delta", map[string]interface{}{
			"type":  "content_block_delta",
			"index": index,
			"delta": delta,
		})
	}
	h.sendSSE(w, flusher, "content_block_stop", map[string]interface{}{
		"type":  "content_block_stop",
		"index": index,
	})
}

func chunkByRunes(s string, size int) []string {
	runes := []rune(s)
	if len(runes) == 0 {
		return nil
	}
	if size <= 0 {
		return []string{s}
	}
	chunks := make([]string, 0, (len(runes)+size-1)/size)
	for i := 0; i < len(runes); i += size {
		end := i + size
		if end > len(runes) {
			end = len(runes)
		}
		chunks = append(chunks, string(runes[i:end]))
	}
	return chunks
}

func estimateReturnedClaudeBlocks(blocks []map[string]interface{}) int {
	var text, reasoning strings.Builder
	var tools []KiroToolUse
	for _, b := range blocks {
		switch b["type"] {
		case "text":
			s, _ := b["text"].(string)
			text.WriteString(s)
		case "thinking":
			s, _ := b["thinking"].(string)
			reasoning.WriteString(s)
		case "tool_use":
			input, _ := b["input"].(map[string]interface{})
			tools = append(tools, KiroToolUse{Input: input})
		}
	}
	return estimateClaudeOutputTokens(text.String(), reasoning.String(), tools)
}
