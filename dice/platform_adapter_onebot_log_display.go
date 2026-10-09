package dice

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"sealdice-core/model"
)

const onebotBridgeLogDisplayMaxIdentities = 10_000

var (
	errOnebotBridgeLogDisplayIndexTooLarge = errors.New("log display index exceeds 10000 identities")
	onebotBridgeMarkdownMentionPattern     = regexp.MustCompile(`\[(@[^\]\r\n]{1,256})\]\((mqqapi://markdown/mention\?[^()\r\n]+)\)`)
	onebotBridgeNativeMentionPattern       = regexp.MustCompile(`<@!?([A-Za-z0-9_-]{1,128})>`)
	onebotBridgeCQAtPattern                = regexp.MustCompile(`\[CQ:at,qq=([A-Za-z0-9_-]{1,128})(?:,name=([^\]\r\n]{0,256}))?\]`)
)

type onebotBridgeLogAuthor struct {
	identity string
	name     string
}

type onebotBridgeLogMentionToken struct {
	start   int
	end     int
	aliases []string
	label   string
	isAll   bool
}

type onebotBridgeLogSnapshotWriter struct {
	out                 io.Writer
	format              string
	authors             map[string]onebotBridgeLogAuthor
	aliasParent         map[string]string
	aliasClaims         map[string]map[string]struct{}
	componentClaims     map[string]map[string]struct{}
	mentionedAliases    map[string]struct{}
	botEvidenceAliases  map[string]struct{}
	humanAuthorAliases  map[string]struct{}
	botEvidenceRoots    map[string]struct{}
	identities          map[string]struct{}
	extraTargets        map[string]struct{}
	anonymous           map[string]string
	knownBotAliasRoots  map[string]struct{}
	nextMember          int
	aliasIndexFinalized bool
}

func newOnebotBridgeLogSnapshotWriter(out io.Writer, format string) *onebotBridgeLogSnapshotWriter {
	return &onebotBridgeLogSnapshotWriter{
		out:                out,
		format:             format,
		authors:            make(map[string]onebotBridgeLogAuthor),
		aliasParent:        make(map[string]string),
		aliasClaims:        make(map[string]map[string]struct{}),
		componentClaims:    make(map[string]map[string]struct{}),
		mentionedAliases:   make(map[string]struct{}),
		botEvidenceAliases: make(map[string]struct{}),
		humanAuthorAliases: make(map[string]struct{}),
		botEvidenceRoots:   make(map[string]struct{}),
		identities:         make(map[string]struct{}),
		extraTargets:       make(map[string]struct{}),
		anonymous:          make(map[string]string),
		knownBotAliasRoots: make(map[string]struct{}),
	}
}

func (w *onebotBridgeLogSnapshotWriter) indexPage(items []*model.LogOneItem) error {
	for _, item := range items {
		if item == nil {
			continue
		}
		identity := onebotBridgeLogIdentity(item)
		if _, exists := w.identities[identity]; !exists {
			if len(w.identities) >= onebotBridgeLogDisplayMaxIdentities {
				return errOnebotBridgeLogDisplayIndexTooLarge
			}
			w.identities[identity] = struct{}{}
		}
		display := onebotBridgeItemDisplay(item)
		name := onebotBridgeSafeSpeakerName(item, display)
		author, exists := w.authors[identity]
		if !exists {
			author = onebotBridgeLogAuthor{identity: identity}
		}
		if name != "" {
			author.name = name
		}
		w.authors[identity] = author
		if display == nil {
			continue
		}
		if err := w.linkAliasGroup(display.AuthorAliases, false); err != nil {
			return err
		}
		for _, alias := range display.AuthorAliases {
			claims := w.aliasClaims[alias]
			if claims == nil {
				claims = make(map[string]struct{})
				w.aliasClaims[alias] = claims
			}
			claims[identity] = struct{}{}
			if item.IsDice {
				w.botEvidenceAliases[alias] = struct{}{}
			} else {
				w.humanAuthorAliases[alias] = struct{}{}
			}
		}
		for _, mention := range display.Mentions {
			aliases := append([]string{mention.Target}, mention.Aliases...)
			if err := w.linkAliasGroup(aliases, true); err != nil {
				return err
			}
			if mention.IsBot {
				for _, alias := range aliases {
					w.botEvidenceAliases[alias] = struct{}{}
				}
			}
		}
	}
	return nil
}

func (w *onebotBridgeLogSnapshotWriter) writePage(items []*model.LogOneItem) error {
	if !w.aliasIndexFinalized {
		if err := w.finalizeAliasIndex(); err != nil {
			return err
		}
		w.aliasIndexFinalized = true
	}
	for _, item := range items {
		if item == nil {
			continue
		}
		if len(item.Message) > onebotBridgeArtifactMaxBytes || len(item.Nickname) > onebotBridgeArtifactMaxBytes || len(item.IMUserID) > onebotBridgeArtifactMaxBytes {
			return errOnebotBridgeArtifactTooLarge
		}
		kind := onebotBridgeItemKind(item)
		timestamp := time.Unix(item.Time, 0).Format("2006-01-02 15:04:05")
		identity := item.UniformID
		if identity == "" {
			identity = item.IMUserID
		}
		itemDisplay := onebotBridgeItemDisplay(item)
		nickname := onebotBridgeSafeSpeakerName(item, itemDisplay)
		if nickname == "" {
			if name, ok := w.authorNameForItem(item); ok {
				nickname = name
			}
		}
		if nickname == "" {
			if item.IsDice && kind != "gap" {
				nickname = "机器人"
			} else if kind == "gap" {
				nickname = "SeaDice记录系统"
			} else {
				label, err := w.anonymousLabel("identity:"+onebotBridgeLogIdentity(item), false)
				if err != nil {
					return err
				}
				nickname = label
			}
		}
		var row strings.Builder
		if kind == "gap" && w.format == "md" {
			row.WriteString("**记录缺口**\n")
		}
		if w.format == "md" {
			color := onebotBridgeMarkdownColor(identity)
			fmt.Fprintf(&row, "### <span style=\"color:%s\">%s</span> — %s", color, onebotBridgeEscapeHTMLLine(nickname), timestamp)
			if item.IsDice {
				row.WriteString(" · **bot**")
			}
			var body string
			var err error
			if item.IsDice && kind != "gap" {
				body, err = w.renderBotMarkdownBody(item.Message, itemDisplay)
			} else {
				body, err = w.renderMarkdownBody(item.Message, itemDisplay, color)
			}
			if err != nil {
				return err
			}
			fmt.Fprintf(&row, "\n\n%s\n\n", body)
		} else {
			botLabel := "member"
			if item.IsDice {
				botLabel = "bot"
			}
			fmt.Fprintf(&row, "%s\t%s\t%s\n", timestamp, strings.NewReplacer("\r", " ", "\n", " ").Replace(nickname), botLabel)
			if kind == "gap" {
				row.WriteString("[记录缺口]\n")
			}
			row.WriteString(item.Message)
			row.WriteString("\n\n")
		}
		if _, err := io.WriteString(w.out, row.String()); err != nil {
			return err
		}
	}
	return nil
}

func onebotBridgeWriteLogHeader(out io.Writer, name, format string) error {
	if format == "md" {
		_, err := io.WriteString(out, "# "+onebotBridgeEscapeTitle(name)+"\n\n")
		return err
	}
	_, err := fmt.Fprintf(out, "日志：%s\n\n", name)
	return err
}

func onebotBridgeItemKind(item *model.LogOneItem) string {
	if info, ok := item.CommandInfo.(map[string]interface{}); ok {
		if value, ok := info["bridgeCaptureKind"].(string); ok && value != "" {
			return value
		}
	}
	return "message"
}

func onebotBridgeItemDisplay(item *model.LogOneItem) *onebotBridgeLogDisplay {
	if item == nil {
		return nil
	}
	info, ok := item.CommandInfo.(map[string]interface{})
	if !ok {
		return nil
	}
	value, ok := info["bridgeDisplay"]
	if !ok {
		return nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	return parseOnebotBridgeLogDisplay(encoded)
}

func onebotBridgeLogIdentity(item *model.LogOneItem) string {
	if item == nil {
		return "unknown"
	}
	if item.UniformID != "" {
		return "uniform:" + item.UniformID
	}
	if item.IMUserID != "" {
		return "user:" + item.IMUserID
	}
	return "row:" + strconv.FormatUint(item.ID, 10)
}

func onebotBridgeUsableLogName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" || !validOnebotBridgeLogDisplayName(name) {
		return ""
	}
	return name
}

func onebotBridgeItemAliases(display *onebotBridgeLogDisplay) []string {
	if display == nil {
		return nil
	}
	return display.AuthorAliases
}

func onebotBridgeSafeSpeakerName(item *model.LogOneItem, display *onebotBridgeLogDisplay) string {
	if item == nil {
		return ""
	}
	name := onebotBridgeUsableLogName(item.Nickname)
	if name == "" || onebotBridgeNameIsIdentity(name, onebotBridgeItemAliases(display)) {
		return ""
	}
	if name == item.IMUserID || name == item.UniformID {
		return ""
	}
	if _, suffix, ok := strings.Cut(item.UniformID, ":"); ok {
		if lastColon := strings.LastIndex(suffix, ":"); lastColon >= 0 {
			suffix = suffix[lastColon+1:]
		}
		if name == suffix {
			return ""
		}
	}
	return name
}

func onebotBridgeNameIsIdentity(name string, aliases []string) bool {
	name = strings.TrimPrefix(strings.TrimSpace(name), "@")
	if name == "" {
		return false
	}
	for _, alias := range aliases {
		if name == alias {
			return true
		}
		_, identifier, ok := strings.Cut(alias, ":")
		if ok && name == identifier {
			return true
		}
	}
	return false
}

func (w *onebotBridgeLogSnapshotWriter) authorNameForItem(item *model.LogOneItem) (string, bool) {
	display := onebotBridgeItemDisplay(item)
	if display == nil {
		return "", false
	}
	identity := onebotBridgeLogIdentity(item)
	for _, alias := range display.AuthorAliases {
		root := w.findAlias(alias)
		claims := w.componentClaims[root]
		if len(claims) != 1 {
			continue
		}
		if _, belongsToItem := claims[identity]; !belongsToItem {
			continue
		}
		if author, ok := w.authors[identity]; ok && author.name != "" {
			return author.name, true
		}
	}
	return "", false
}

func (w *onebotBridgeLogSnapshotWriter) linkAliasGroup(aliases []string, isTarget bool) error {
	unique := make([]string, 0, len(aliases))
	seen := make(map[string]struct{}, len(aliases))
	for _, alias := range aliases {
		if _, exists := seen[alias]; exists {
			continue
		}
		seen[alias] = struct{}{}
		if _, exists := w.aliasParent[alias]; !exists {
			if len(w.aliasParent) >= onebotBridgeLogDisplayMaxIdentities*8 {
				return errOnebotBridgeLogDisplayIndexTooLarge
			}
			w.aliasParent[alias] = alias
		}
		unique = append(unique, alias)
		if isTarget {
			w.mentionedAliases[alias] = struct{}{}
		}
	}
	if len(unique) > 1 {
		for _, alias := range unique[1:] {
			w.unionAliases(unique[0], alias)
		}
	}
	return nil
}

func (w *onebotBridgeLogSnapshotWriter) findAlias(alias string) string {
	root := alias
	for {
		parent, ok := w.aliasParent[root]
		if !ok || parent == root {
			break
		}
		root = parent
	}
	for current := alias; ; {
		parent, ok := w.aliasParent[current]
		if !ok || parent == current {
			break
		}
		w.aliasParent[current] = root
		current = parent
	}
	return root
}

func (w *onebotBridgeLogSnapshotWriter) unionAliases(left, right string) {
	leftRoot := w.findAlias(left)
	rightRoot := w.findAlias(right)
	if leftRoot == rightRoot {
		return
	}
	if leftRoot < rightRoot {
		w.aliasParent[rightRoot] = leftRoot
	} else {
		w.aliasParent[leftRoot] = rightRoot
	}
}

func (w *onebotBridgeLogSnapshotWriter) finalizeAliasIndex() error {
	w.componentClaims = make(map[string]map[string]struct{})
	for alias, claims := range w.aliasClaims {
		root := w.findAlias(alias)
		component := w.componentClaims[root]
		if component == nil {
			component = make(map[string]struct{})
			w.componentClaims[root] = component
		}
		for identity := range claims {
			component[identity] = struct{}{}
		}
	}
	w.botEvidenceRoots = make(map[string]struct{})
	for alias := range w.botEvidenceAliases {
		w.botEvidenceRoots[w.findAlias(alias)] = struct{}{}
	}
	humanAuthorRoots := make(map[string]struct{}, len(w.humanAuthorAliases))
	for alias := range w.humanAuthorAliases {
		humanAuthorRoots[w.findAlias(alias)] = struct{}{}
	}
	w.knownBotAliasRoots = make(map[string]struct{})
	for root := range w.botEvidenceRoots {
		_, hasHumanAuthor := humanAuthorRoots[root]
		if len(w.componentClaims[root]) <= 1 && !hasHumanAuthor {
			w.knownBotAliasRoots[root] = struct{}{}
		}
	}
	for alias := range w.mentionedAliases {
		root := w.findAlias(alias)
		if len(w.componentClaims[root]) == 0 {
			w.extraTargets["target:"+root] = struct{}{}
		}
	}
	if len(w.identities)+len(w.extraTargets) > onebotBridgeLogDisplayMaxIdentities {
		return errOnebotBridgeLogDisplayIndexTooLarge
	}
	return nil
}

func (w *onebotBridgeLogSnapshotWriter) authorForAliases(aliases []string) (onebotBridgeLogAuthor, bool) {
	identities := make(map[string]struct{})
	for _, alias := range aliases {
		root := w.findAlias(alias)
		claims := w.componentClaims[root]
		if len(claims) > 1 {
			return onebotBridgeLogAuthor{}, false
		}
		for identity := range claims {
			identities[identity] = struct{}{}
		}
	}
	if len(identities) != 1 {
		return onebotBridgeLogAuthor{}, false
	}
	for identity := range identities {
		author, ok := w.authors[identity]
		return author, ok
	}
	return onebotBridgeLogAuthor{}, false
}

func (w *onebotBridgeLogSnapshotWriter) anonymousLabel(identity string, countTarget bool) (string, error) {
	if label, ok := w.anonymous[identity]; ok {
		return label, nil
	}
	if countTarget {
		if _, exists := w.extraTargets[identity]; !exists && len(w.identities)+len(w.extraTargets) >= onebotBridgeLogDisplayMaxIdentities {
			return "", errOnebotBridgeLogDisplayIndexTooLarge
		}
		w.extraTargets[identity] = struct{}{}
	}
	w.nextMember++
	label := "成员" + strconv.Itoa(w.nextMember)
	w.anonymous[identity] = label
	return label, nil
}

func (w *onebotBridgeLogSnapshotWriter) renderMarkdownBody(text string, display *onebotBridgeLogDisplay, color string) (string, error) {
	out := &onebotBridgeLimitedBuffer{}
	if _, err := out.WriteString("<div>"); err != nil {
		return "", err
	}
	writeLine := func(line string, index int) error {
		if index > 0 {
			if _, err := out.WriteString("<br />"); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintf(out, "<span style=\"white-space:pre-wrap;color:%s\">", color); err != nil {
			return err
		}
		if err := w.renderMarkdownLine(out, line, display); err != nil {
			return err
		}
		_, err := out.WriteString("</span>")
		return err
	}
	lineStart := 0
	lineIndex := 0
	for index := 0; index < len(text); index++ {
		if text[index] != '\r' && text[index] != '\n' {
			continue
		}
		if err := writeLine(text[lineStart:index], lineIndex); err != nil {
			return "", err
		}
		lineIndex++
		if text[index] == '\r' && index+1 < len(text) && text[index+1] == '\n' {
			index++
		}
		lineStart = index + 1
	}
	if err := writeLine(text[lineStart:], lineIndex); err != nil {
		return "", err
	}
	if _, err := out.WriteString("</div>"); err != nil {
		return "", err
	}
	return out.String(), nil
}

func (w *onebotBridgeLogSnapshotWriter) renderMarkdownLine(out io.StringWriter, text string, display *onebotBridgeLogDisplay) error {
	tokens := onebotBridgeParseMentionTokens(text)
	if len(tokens) == 0 {
		return onebotBridgeWriteEscapedHTML(out, text)
	}
	last := 0
	for _, token := range tokens {
		if token.start < last || token.end > len(text) {
			continue
		}
		if err := onebotBridgeWriteEscapedHTML(out, text[last:token.start]); err != nil {
			return err
		}
		name, err := w.resolveMention(token, display)
		if err != nil {
			return err
		}
		if err := onebotBridgeWriteEscapedHTML(out, "@"+name); err != nil {
			return err
		}
		last = token.end
	}
	return onebotBridgeWriteEscapedHTML(out, text[last:])
}

func onebotBridgeWriteEscapedHTML(out io.StringWriter, value string) error {
	start := 0
	for index := range len(value) {
		var entity string
		switch value[index] {
		case '&':
			entity = "&amp;"
		case '<':
			entity = "&lt;"
		case '>':
			entity = "&gt;"
		case '"':
			entity = "&#34;"
		case '\'':
			entity = "&#39;"
		case '\r':
			entity = "&#13;"
		default:
			continue
		}
		if _, err := out.WriteString(value[start:index]); err != nil {
			return err
		}
		if _, err := out.WriteString(entity); err != nil {
			return err
		}
		start = index + 1
	}
	_, err := out.WriteString(value[start:])
	return err
}

func (w *onebotBridgeLogSnapshotWriter) resolveMention(token onebotBridgeLogMentionToken, display *onebotBridgeLogDisplay) (string, error) {
	if token.isAll {
		return "全体成员", nil
	}
	current := onebotBridgeCurrentMention(display, token.aliases)
	currentAliases := append([]string(nil), token.aliases...)
	if current != nil {
		currentAliases = append(currentAliases, current.Target)
		currentAliases = append(currentAliases, current.Aliases...)
	}
	ambiguous := w.aliasesHaveConflictingAuthors(currentAliases)
	root := ""
	if len(currentAliases) > 0 {
		root = w.findAlias(currentAliases[0])
	}
	if root != "" {
		if _, hasBotEvidence := w.botEvidenceRoots[root]; hasBotEvidence {
			if _, confirmedBot := w.knownBotAliasRoots[root]; confirmedBot {
				return "机器人", nil
			}
			return w.anonymousTarget(root)
		}
	}
	if !ambiguous && current != nil && current.IsBot {
		return "机器人", nil
	}
	if !ambiguous && current != nil {
		if name := onebotBridgeUsableLogName(current.Name); name != "" && !onebotBridgeNameIsIdentity(name, currentAliases) {
			return strings.TrimPrefix(name, "@"), nil
		}
	}
	if token.label != "" && !onebotBridgeNameIsIdentity(token.label, currentAliases) {
		return strings.TrimPrefix(token.label, "@"), nil
	}
	authorAliases := append([]string(nil), token.aliases...)
	if current != nil {
		authorAliases = append(authorAliases, current.Target)
		authorAliases = append(authorAliases, current.Aliases...)
	}
	identityLabel := ""
	if author, ok := w.authorForAliases(authorAliases); ok {
		if author.name != "" && !onebotBridgeNameIsIdentity(author.name, authorAliases) {
			return strings.TrimPrefix(author.name, "@"), nil
		}
		identityLabel = "identity:" + author.identity
	}
	if identityLabel != "" {
		label, err := w.anonymousLabel(identityLabel, false)
		return label, err
	}
	canonicalAliases := append([]string(nil), token.aliases...)
	if current != nil {
		canonicalAliases = append(canonicalAliases, current.Target)
		canonicalAliases = append(canonicalAliases, current.Aliases...)
	}
	identity := "unknown-target"
	if len(canonicalAliases) > 0 {
		identity = w.findAlias(canonicalAliases[0])
	}
	return w.anonymousTarget(identity)
}

func (w *onebotBridgeLogSnapshotWriter) anonymousTarget(root string) (string, error) {
	identityKey := "target:" + root
	countTarget := len(w.componentClaims[root]) == 0
	return w.anonymousLabel(identityKey, countTarget)
}

func (w *onebotBridgeLogSnapshotWriter) aliasesHaveConflictingAuthors(aliases []string) bool {
	identities := make(map[string]struct{})
	for _, alias := range aliases {
		claims := w.componentClaims[w.findAlias(alias)]
		if len(claims) > 1 {
			return true
		}
		for identity := range claims {
			identities[identity] = struct{}{}
		}
		if len(identities) > 1 {
			return true
		}
	}
	return false
}

func onebotBridgeCurrentMention(display *onebotBridgeLogDisplay, aliases []string) *onebotBridgeLogMention {
	if display == nil || len(aliases) == 0 {
		return nil
	}
	wanted := make(map[string]struct{}, len(aliases))
	for _, alias := range aliases {
		wanted[alias] = struct{}{}
	}
	var found *onebotBridgeLogMention
	for index := range display.Mentions {
		mention := &display.Mentions[index]
		matches := false
		if _, ok := wanted[mention.Target]; ok {
			matches = true
		}
		for _, alias := range mention.Aliases {
			if _, ok := wanted[alias]; ok {
				matches = true
				break
			}
		}
		if !matches {
			continue
		}
		if found != nil && (found.Target != mention.Target || found.Name != mention.Name || found.IsBot != mention.IsBot) {
			return nil
		}
		found = mention
	}
	return found
}

func onebotBridgeParseMentionTokens(text string) []onebotBridgeLogMentionToken {
	tokens := make([]onebotBridgeLogMentionToken, 0)
	for _, match := range onebotBridgeMarkdownMentionPattern.FindAllStringSubmatchIndex(text, -1) {
		if len(match) < 6 {
			continue
		}
		label := text[match[2]:match[3]]
		urlText := text[match[4]:match[5]]
		if !strings.HasPrefix(urlText, "mqqapi://markdown/mention?") {
			continue
		}
		target, ok := onebotBridgeMarkdownTarget(urlText)
		if !ok {
			digest := sha256.Sum256([]byte(urlText))
			target = fmt.Sprintf("opaque:%x", digest[:8])
			label = ""
		} else if !validOnebotBridgeLogDisplayName(label) || !strings.HasPrefix(label, "@") || label == "@" {
			label = ""
		}
		tokens = append(tokens, onebotBridgeLogMentionToken{start: match[0], end: match[1], aliases: []string{target}, label: label})
	}
	for _, match := range onebotBridgeNativeMentionPattern.FindAllStringSubmatchIndex(text, -1) {
		id := text[match[2]:match[3]]
		tokens = append(tokens, onebotBridgeLogMentionToken{start: match[0], end: match[1], aliases: []string{"openid:" + id}})
	}
	for _, match := range onebotBridgeCQAtPattern.FindAllStringSubmatchIndex(text, -1) {
		id := text[match[2]:match[3]]
		if id == "all" {
			tokens = append(tokens, onebotBridgeLogMentionToken{start: match[0], end: match[1], isAll: true})
			continue
		}
		var label string
		if match[4] >= 0 && match[5] >= 0 {
			label = text[match[4]:match[5]]
			if label != "" && !validOnebotBridgeLogDisplayName(label) {
				label = ""
			}
		}
		tokens = append(tokens, onebotBridgeLogMentionToken{start: match[0], end: match[1], aliases: []string{"openid:" + id}, label: label})
	}
	sort.Slice(tokens, func(i, j int) bool {
		if tokens[i].start == tokens[j].start {
			return tokens[i].end > tokens[j].end
		}
		return tokens[i].start < tokens[j].start
	})
	result := tokens[:0]
	last := -1
	for _, token := range tokens {
		if token.start < last {
			continue
		}
		result = append(result, token)
		last = token.end
	}
	return result
}

func onebotBridgeMarkdownTarget(rawURL string) (string, bool) {
	if len(rawURL) > 2048 {
		return "", false
	}
	rawURL = strings.ReplaceAll(rawURL, "&amp;", "&")
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme != "mqqapi" || parsed.Host != "markdown" || parsed.Path != "/mention" ||
		parsed.User != nil || parsed.Opaque != "" || parsed.Fragment != "" || parsed.Port() != "" || parsed.RawQuery == "" {
		return "", false
	}
	query, err := url.ParseQuery(parsed.RawQuery)
	if err != nil || len(query["at_type"]) != 1 || len(query["at_tinyid"]) != 1 || query.Get("at_type") != "1" ||
		!onebotBridgeLogTinyIDPattern.MatchString(query.Get("at_tinyid")) {
		return "", false
	}
	return "tinyid:" + query.Get("at_tinyid"), true
}

func onebotBridgeEscapeHTMLLine(value string) string {
	value = html.EscapeString(value)
	return strings.NewReplacer("\r", "&#13;", "\n", "&#10;").Replace(value)
}
