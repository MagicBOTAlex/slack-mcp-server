package handler

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gocarina/gocsv"
	"github.com/korotovsky/slack-mcp-server/pkg/limiter"
	"github.com/korotovsky/slack-mcp-server/pkg/provider"
	"github.com/korotovsky/slack-mcp-server/pkg/provider/edge"
	"github.com/korotovsky/slack-mcp-server/pkg/server/auth"
	"github.com/korotovsky/slack-mcp-server/pkg/text"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/slack-go/slack"
	slackGoUtil "github.com/takara2314/slack-go-util"
	"go.uber.org/zap"
)

const (
	defaultConversationsNumericLimit    = 50
	defaultThreadNumericLimit           = 200
	defaultConversationsExpressionLimit = "1d"
	maxFileSizeBytes                    = 5 * 1024 * 1024 // 5MB limit
	maxThreadSearchMessages             = 5000            // safety cap when scanning a whole thread
	maxExpandedHistoryMessages          = 1000            // safety cap for include_thread_replies
)

var validFilterKeys = map[string]struct{}{
	"is":     {},
	"in":     {},
	"from":   {},
	"with":   {},
	"before": {},
	"after":  {},
	"on":     {},
	"during": {},
}

type Message struct {
	MsgID         string `json:"msgID"`
	UserID        string `json:"userID"`
	UserName      string `json:"userUser"`
	RealName      string `json:"realName"`
	Channel       string `json:"channelID"`
	ThreadTs      string `json:"ThreadTs"`
	Text          string `json:"text"`
	Time          string `json:"time"`
	Permalink     string `json:"permalink,omitempty"`
	Reactions     string `json:"reactions,omitempty"`
	ReactionUsers string `json:"reactionUsers,omitempty"`
	BotName       string `json:"botName,omitempty"`
	FileCount     int    `json:"fileCount,omitempty"`
	AttachmentIDs string `json:"attachmentIDs,omitempty"`
	HasMedia      bool   `json:"hasMedia,omitempty"`
	ReplyCount    int    `json:"replyCount,omitempty"`
	ReplyUsers    int    `json:"replyUsersCount,omitempty"`
	LatestReply   string `json:"latestReply,omitempty"`
	Cursor        string `json:"cursor"`
}

type User struct {
	UserID   string `json:"userID"`
	UserName string `json:"userName"`
	RealName string `json:"realName"`
}

type UserSearchResult struct {
	UserID         string `csv:"UserID"`
	UserName       string `csv:"UserName"`
	RealName       string `csv:"RealName"`
	DisplayName    string `csv:"DisplayName"`
	Email          string `csv:"Email"`
	Title          string `csv:"Title"`
	TimeZone       string `csv:"TimeZone"`
	TimeZoneLabel  string `csv:"TimeZoneLabel"`
	TimeZoneOffset int    `csv:"TimeZoneOffset"`
	LocalTime      string `csv:"LocalTime"`
	Presence       string `csv:"Presence"`
	StatusText     string `csv:"StatusText"`
	StatusEmoji    string `csv:"StatusEmoji"`
	Phone          string `csv:"Phone"`
	DMChannelID    string `csv:"DMChannelID"`
}

type ReactionRow struct {
	Reaction string `csv:"Reaction"`
	Count    int    `csv:"Count"`
	Users    string `csv:"Users"`
}

type PinRow struct {
	Type      string `csv:"Type"`
	Channel   string `csv:"Channel"`
	Timestamp string `csv:"Timestamp"`
	UserID    string `csv:"UserID"`
	Text      string `csv:"Text"`
	FileID    string `csv:"FileID"`
	FileName  string `csv:"FileName"`
	Permalink string `csv:"Permalink"`
}

type MemberRow struct {
	UserID   string `json:"userID"`
	UserName string `json:"userName"`
	RealName string `json:"realName"`
	IsBot    bool   `json:"isBot"`
	IsAdmin  bool   `json:"isAdmin"`
}

type FileRow struct {
	ID        string `csv:"ID"`
	Name      string `csv:"Name"`
	Title     string `csv:"Title"`
	FileType  string `csv:"FileType"`
	Mimetype  string `csv:"Mimetype"`
	Size      int    `csv:"Size"`
	UserID    string `csv:"UserID"`
	Permalink string `csv:"Permalink"`
}

type conversationParams struct {
	channel              string
	threadTs             string
	query                string
	limit                int
	oldest               string
	latest               string
	cursor               string
	activity             bool
	includeThreadReplies bool
}

type searchParams struct {
	query string
	limit int
	page  int
	// threadTs restricts the search to a single thread. threadChannel is the
	// channel that owns that thread (resolved from filter_in_channel).
	threadTs      string
	threadChannel string
	// messageRef is set when search_query is a full Slack message URL.
	messageRef *slackMessageRef
	// freeText and filters are the parsed parts of search_query. They are used
	// for thread-local filtering, where Slack's search API cannot help.
	freeText []string
	filters  map[string][]string
	// cursor is the mode-specific cursor decoded from the request cursor.
	cursor     string
	cursorMode string
	// dateLocation is set when filter_date_timezone is supplied. It is used
	// for thread-local filtering and for filtering search results client-side.
	dateLocation *time.Location
}

// slackMessageRef is a channel/message/thread reference parsed from a Slack
// permalink such as
// https://example.slack.com/archives/C123/p1234567890123456?thread_ts=1234567890.123456
type slackMessageRef struct {
	ChannelID string
	Timestamp string
	ThreadTS  string
}

// rootTS returns the timestamp that identifies the thread containing the
// referenced message. For a top-level message that is the message itself.
func (r *slackMessageRef) rootTS() string {
	if r.ThreadTS != "" {
		return r.ThreadTS
	}
	return r.Timestamp
}

type addMessageParams struct {
	channel     string
	threadTs    string
	text        string
	contentType string
	blocks      []slack.Block
}

type addReactionParams struct {
	channel   string
	timestamp string
	emoji     string
}

type filesGetParams struct {
	fileID string
}

type usersSearchParams struct {
	query string
	limit int
}

type conversationInfoParams struct {
	channel string
}

type reactionsListParams struct {
	channel   string
	timestamp string
}

type pinsListParams struct {
	channel string
}

type membersParams struct {
	channel string
	limit   int
	cursor  string
}

type editMessageParams struct {
	channel     string
	timestamp   string
	text        string
	contentType string
	blocks      []slack.Block
}

type deleteMessageParams struct {
	channel   string
	timestamp string
}

type uploadFileParams struct {
	channel   string
	filename  string
	content   string
	content64 string
	title     string
	comment   string
	threadTs  string
}

type filesListParams struct {
	channel string
	types   string
	limit   int
	page    int
}

type canvasReadParams struct {
	fileID string
}

type listsItemsParams struct {
	listID string
	limit  int
	cursor string
}

type unreadsParams struct {
	includeMessages       bool
	channelTypes          string
	maxChannels           int
	maxMessagesPerChannel int
	mentionsOnly          bool
	includeMuted          bool
	mutedChannels         map[string]bool // populated at runtime from Slack prefs
	mutedUnavailable      bool            // true when muted channels could not be fetched (e.g. xoxp token)
}

type markParams struct {
	channel string
	ts      string
}
type ConversationsHandler struct {
	apiProvider *provider.ApiProvider
	logger      *zap.Logger
}

func NewConversationsHandler(apiProvider *provider.ApiProvider, logger *zap.Logger) *ConversationsHandler {
	return &ConversationsHandler{
		apiProvider: apiProvider,
		logger:      logger,
	}
}

// UsersResource streams a CSV of all users
func (ch *ConversationsHandler) UsersResource(ctx context.Context, request mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
	ch.logger.Debug("UsersResource called", zap.Any("params", request.Params))

	// authentication
	if authenticated, err := auth.IsAuthenticated(ctx, ch.apiProvider.ServerTransport(), ch.logger); !authenticated {
		ch.logger.Error("Authentication failed for users resource", zap.Error(err))
		return nil, err
	}

	// provider readiness
	if ready, err := ch.apiProvider.IsReady(); !ready {
		ch.logger.Error("API provider not ready", zap.Error(err))
		return nil, err
	}

	// Slack auth test
	ar, err := ch.apiProvider.Slack().AuthTest()
	if err != nil {
		ch.logger.Error("Slack AuthTest failed", zap.Error(err))
		return nil, err
	}

	ws, err := text.Workspace(ar.URL)
	if err != nil {
		ch.logger.Error("Failed to parse workspace from URL",
			zap.String("url", ar.URL),
			zap.Error(err),
		)
		return nil, fmt.Errorf("failed to parse workspace from URL: %v", err)
	}

	// collect users
	usersMaps := ch.apiProvider.ProvideUsersMap()
	users := usersMaps.Users
	usersList := make([]User, 0, len(users))
	for _, user := range users {
		usersList = append(usersList, User{
			UserID:   user.ID,
			UserName: user.Name,
			RealName: user.RealName,
		})
	}

	// marshal CSV
	csvBytes, err := gocsv.MarshalBytes(&usersList)
	if err != nil {
		ch.logger.Error("Failed to marshal users to CSV", zap.Error(err))
		return nil, err
	}

	return []mcp.ResourceContents{
		mcp.TextResourceContents{
			URI:      "slack://" + ws + "/users",
			MIMEType: "text/csv",
			Text:     string(csvBytes),
		},
	}, nil
}

// ConversationsAddMessageHandler posts a message and returns a confirmation
func (ch *ConversationsHandler) ConversationsAddMessageHandler(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ch.logger.Debug("ConversationsAddMessageHandler called", zap.Any("params", request.Params))

	// provider readiness
	if ready, err := ch.apiProvider.IsReady(); !ready {
		ch.logger.Error("API provider not ready", zap.Error(err))
		return nil, err
	}

	params, err := ch.parseParamsToolAddMessage(ctx, request)
	if err != nil {
		ch.logger.Error("Failed to parse add-message params", zap.Error(err))
		return nil, err
	}

	var options []slack.MsgOption
	if params.threadTs != "" {
		options = append(options, slack.MsgOptionTS(params.threadTs))
	}

	if params.blocks != nil {
		// Raw blocks provided: use them directly. If text is also provided, it
		// serves as the notification/fallback text.
		options = append(options, slack.MsgOptionBlocks(params.blocks...))
		if params.text != "" {
			options = append(options, slack.MsgOptionText(params.text, false))
		}
	} else {
		switch params.contentType {
		case "text/plain":
			options = append(options, slack.MsgOptionDisableMarkdown())
			options = append(options, slack.MsgOptionText(params.text, false))
		case "text/markdown":
			blocks, err := slackGoUtil.ConvertMarkdownTextToBlocks(params.text)
			if err != nil {
				ch.logger.Warn("Markdown parsing error", zap.Error(err))
				options = append(options, slack.MsgOptionDisableMarkdown())
				options = append(options, slack.MsgOptionText(params.text, false))
			} else {
				options = append(options, slack.MsgOptionBlocks(blocks...))
			}
		default:
			return nil, errors.New("content_type must be either 'text/plain' or 'text/markdown'")
		}
	}

	unfurlOpt := os.Getenv("SLACK_MCP_ADD_MESSAGE_UNFURLING")
	if text.IsUnfurlingEnabled(params.text, unfurlOpt, ch.logger) {
		options = append(options, slack.MsgOptionEnableLinkUnfurl())
	} else {
		options = append(options, slack.MsgOptionDisableLinkUnfurl())
		options = append(options, slack.MsgOptionDisableMediaUnfurl())
	}

	ch.logger.Debug("Posting Slack message",
		zap.String("channel", params.channel),
		zap.String("thread_ts", params.threadTs),
		zap.String("content_type", params.contentType),
	)
	respChannel, respTimestamp, err := ch.apiProvider.Slack().PostMessageContext(ctx, params.channel, options...)
	if err != nil {
		ch.logger.Error("Slack PostMessageContext failed", zap.Error(err))
		return nil, err
	}

	toolConfig := os.Getenv("SLACK_MCP_ADD_MESSAGE_MARK")
	if toolConfig == "1" || toolConfig == "true" || toolConfig == "yes" {
		err := ch.apiProvider.Slack().MarkConversationContext(ctx, params.channel, respTimestamp)
		if err != nil {
			ch.logger.Error("Slack MarkConversationContext failed", zap.Error(err))
			return nil, err
		}
	}

	if params.threadTs != "" {
		return mcp.NewToolResultText(fmt.Sprintf("Successfully posted message to channel %s in thread %s (ts=%s)", respChannel, params.threadTs, respTimestamp)), nil
	}
	return mcp.NewToolResultText(fmt.Sprintf("Successfully posted message to channel %s (ts=%s)", respChannel, respTimestamp)), nil
}

// ReactionsAddHandler adds an emoji reaction to a message
func (ch *ConversationsHandler) ReactionsAddHandler(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ch.logger.Debug("ReactionsAddHandler called", zap.Any("params", request.Params))

	// provider readiness
	if ready, err := ch.apiProvider.IsReady(); !ready {
		ch.logger.Error("API provider not ready", zap.Error(err))
		return nil, err
	}

	params, err := ch.parseParamsToolReaction(ctx, request)
	if err != nil {
		ch.logger.Error("Failed to parse add-reaction params", zap.Error(err))
		return nil, err
	}

	itemRef := slack.ItemRef{
		Channel:   params.channel,
		Timestamp: params.timestamp,
	}

	ch.logger.Debug("Adding reaction to Slack message",
		zap.String("channel", params.channel),
		zap.String("timestamp", params.timestamp),
		zap.String("emoji", params.emoji),
	)

	err = ch.apiProvider.Slack().AddReactionContext(ctx, params.emoji, itemRef)
	if err != nil {
		ch.logger.Error("Slack AddReactionContext failed", zap.Error(err))
		return nil, err
	}

	return mcp.NewToolResultText(fmt.Sprintf("Successfully added :%s: reaction to message %s in channel %s", params.emoji, params.timestamp, params.channel)), nil
}

// ReactionsRemoveHandler removes an emoji reaction from a message
func (ch *ConversationsHandler) ReactionsRemoveHandler(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ch.logger.Debug("ReactionsRemoveHandler called", zap.Any("params", request.Params))

	// provider readiness
	if ready, err := ch.apiProvider.IsReady(); !ready {
		ch.logger.Error("API provider not ready", zap.Error(err))
		return nil, err
	}

	params, err := ch.parseParamsToolReaction(ctx, request)
	if err != nil {
		ch.logger.Error("Failed to parse remove-reaction params", zap.Error(err))
		return nil, err
	}

	itemRef := slack.ItemRef{
		Channel:   params.channel,
		Timestamp: params.timestamp,
	}

	ch.logger.Debug("Removing reaction from Slack message",
		zap.String("channel", params.channel),
		zap.String("timestamp", params.timestamp),
		zap.String("emoji", params.emoji),
	)

	err = ch.apiProvider.Slack().RemoveReactionContext(ctx, params.emoji, itemRef)
	if err != nil {
		ch.logger.Error("Slack RemoveReactionContext failed", zap.Error(err))
		return nil, err
	}

	return mcp.NewToolResultText(fmt.Sprintf("Successfully removed :%s: reaction from message %s in channel %s", params.emoji, params.timestamp, params.channel)), nil
}

func (ch *ConversationsHandler) UsersSearchHandler(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ch.logger.Debug("UsersSearchHandler called", zap.Any("params", request.Params))

	if ready, err := ch.apiProvider.IsReady(); !ready {
		ch.logger.Error("API provider not ready", zap.Error(err))
		return nil, err
	}

	params, err := ch.parseParamsToolUsersSearch(request)
	if err != nil {
		ch.logger.Error("Failed to parse users-search params", zap.Error(err))
		return nil, err
	}

	ch.logger.Debug("Searching for users",
		zap.String("query", params.query),
		zap.Int("limit", params.limit),
	)

	users, err := ch.apiProvider.SearchUsers(ctx, params.query, params.limit)
	if err != nil {
		ch.logger.Error("UsersSearch failed", zap.Error(err))
		return nil, fmt.Errorf("users search failed: %w", err)
	}

	channelsMap := ch.apiProvider.ProvideChannelsMaps()
	includePresence := request.GetBool("include_presence", false)
	presenceLimiter := limiter.Tier2boost.Limiter()

	results := make([]UserSearchResult, 0, len(users))
	for _, user := range users {
		if user.Deleted {
			continue
		}

		dmChannelID := ""
		for _, ch := range channelsMap.Channels {
			if ch.IsIM && ch.User == user.ID {
				dmChannelID = ch.ID
				break
			}
		}

		localTime := ""
		if user.TZ != "" {
			if loc, err := time.LoadLocation(user.TZ); err == nil {
				localTime = time.Now().In(loc).Format("2006-01-02 15:04 MST")
			}
		}
		presence := ""
		if includePresence {
			if err := presenceLimiter.Wait(ctx); err == nil {
				if p, err := ch.apiProvider.Slack().GetUserPresenceContext(ctx, user.ID); err == nil {
					presence = p.Presence
				}
			}
		}

		results = append(results, UserSearchResult{
			UserID:         user.ID,
			UserName:       user.Name,
			RealName:       user.RealName,
			DisplayName:    user.Profile.DisplayName,
			Email:          user.Profile.Email,
			Title:          user.Profile.Title,
			TimeZone:       user.TZ,
			TimeZoneLabel:  user.TZLabel,
			TimeZoneOffset: user.TZOffset,
			LocalTime:      localTime,
			Presence:       presence,
			StatusText:     user.Profile.StatusText,
			StatusEmoji:    user.Profile.StatusEmoji,
			Phone:          user.Profile.Phone,
			DMChannelID:    dmChannelID,
		})
	}

	if len(results) == 0 {
		return mcp.NewToolResultText("No users found matching the query."), nil
	}

	csvBytes, err := gocsv.MarshalBytes(&results)
	if err != nil {
		ch.logger.Error("Failed to marshal users to CSV", zap.Error(err))
		return nil, err
	}

	return mcp.NewToolResultText(string(csvBytes)), nil
}

// ConversationsInfoHandler returns channel metadata and read state.
func (ch *ConversationsHandler) ConversationsInfoHandler(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	channel := strings.TrimSpace(request.GetString("channel_id", ""))
	if channel == "" {
		return nil, errors.New("channel_id must be a string")
	}
	if ready, err := ch.apiProvider.IsReady(); !ready {
		return nil, err
	}
	id, err := ch.resolveChannelID(ctx, channel)
	if err != nil {
		return nil, err
	}
	info, err := ch.apiProvider.Slack().GetConversationInfoContext(ctx, &slack.GetConversationInfoInput{
		ChannelID:         id,
		IncludeNumMembers: true,
	})
	if err != nil {
		return nil, err
	}

	result := map[string]any{
		"id":                   info.ID,
		"name":                 info.Name,
		"is_channel":           info.IsChannel,
		"is_private":           info.IsPrivate,
		"is_archived":          info.IsArchived,
		"is_general":           info.IsGeneral,
		"is_member":            info.IsMember,
		"is_im":                info.IsIM,
		"is_mpim":              info.IsMpIM,
		"is_shared":            info.IsShared,
		"is_ext_shared":        info.IsExtShared,
		"is_org_shared":        info.IsOrgShared,
		"num_members":          info.NumMembers,
		"creator":              info.Creator,
		"created":              info.Created,
		"topic":                info.Topic.Value,
		"purpose":              info.Purpose.Value,
		"last_read":            info.LastRead,
		"unread_count":         info.UnreadCount,
		"unread_count_display": info.UnreadCountDisplay,
	}
	if info.Latest != nil {
		result["latest"] = info.Latest.Timestamp
	}
	data, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	return mcp.NewToolResultText(string(data)), nil
}

func (ch *ConversationsHandler) ReactionsListHandler(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	params, err := ch.parseParamsToolReactionsList(ctx, request)
	if err != nil {
		return nil, err
	}
	reacted, err := ch.apiProvider.Slack().GetReactionsContext(ctx, slack.ItemRef{
		Channel:   params.channel,
		Timestamp: params.timestamp,
	}, slack.GetReactionsParameters{Full: true})
	if err != nil {
		return nil, err
	}
	if len(reacted.Reactions) == 0 {
		return mcp.NewToolResultText("No reactions found."), nil
	}

	resolver := ch.newUserResolver(ctx)
	rows := make([]ReactionRow, 0, len(reacted.Reactions))
	for _, r := range reacted.Reactions {
		names := make([]string, 0, len(r.Users))
		for _, uid := range r.Users {
			name, _, _ := resolver.resolve(uid)
			if name == "" {
				name = uid
			}
			names = append(names, name)
		}
		rows = append(rows, ReactionRow{Reaction: r.Name, Count: r.Count, Users: strings.Join(names, ",")})
	}
	return marshalRowsToCSV(rows)
}

func (ch *ConversationsHandler) PinsListHandler(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	params, err := ch.parseParamsToolPinsList(ctx, request)
	if err != nil {
		return nil, err
	}
	items, _, err := ch.apiProvider.Slack().ListPinsContext(ctx, params.channel)
	if err != nil {
		return nil, err
	}
	baseURL := ch.workspaceBaseURL()
	rows := make([]PinRow, 0, len(items))
	for _, item := range items {
		row := PinRow{Type: item.Type, Channel: item.Channel, Timestamp: item.Timestamp}
		if item.Message != nil {
			row.UserID = item.Message.User
			row.Text = text.ProcessText(item.Message.Text)
			row.Timestamp = item.Message.Timestamp
			row.Permalink = slackPermalink(baseURL, item.Channel, item.Message.Timestamp, item.Message.ThreadTimestamp)
		}
		if item.File != nil {
			row.FileID = item.File.ID
			row.FileName = item.File.Name
			row.Permalink = item.File.Permalink
		}
		rows = append(rows, row)
	}
	return marshalRowsToCSV(rows)
}

func (ch *ConversationsHandler) ChannelMembersHandler(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	params, err := ch.parseParamsToolMembers(ctx, request)
	if err != nil {
		return nil, err
	}
	if ready, err := ch.apiProvider.IsReady(); !ready {
		return nil, err
	}
	ids, nextCursor, err := ch.apiProvider.Slack().GetUsersInConversationContext(ctx, &slack.GetUsersInConversationParameters{
		ChannelID: params.channel,
		Limit:     params.limit,
		Cursor:    params.cursor,
	})
	if err != nil {
		return nil, err
	}

	usersMap := ch.apiProvider.ProvideUsersMap()
	members := make([]MemberRow, 0, len(ids))
	for _, id := range ids {
		row := MemberRow{UserID: id, UserName: id, RealName: id}
		if u, ok := usersMap.Users[id]; ok {
			row.UserName = u.Name
			row.RealName = u.RealName
			row.IsBot = u.IsBot
			row.IsAdmin = u.IsAdmin
		}
		members = append(members, row)
	}
	data, err := json.Marshal(map[string]any{
		"channel_id":  params.channel,
		"members":     members,
		"next_cursor": nextCursor,
	})
	if err != nil {
		return nil, err
	}
	return mcp.NewToolResultText(string(data)), nil
}

func (ch *ConversationsHandler) ConversationsEditMessageHandler(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	params, err := ch.parseParamsToolEditMessage(ctx, request)
	if err != nil {
		return nil, err
	}
	options, err := buildMessageOptions(params.text, params.contentType, params.blocks)
	if err != nil {
		return nil, err
	}
	channel, ts, _, err := ch.apiProvider.Slack().UpdateMessageContext(ctx, params.channel, params.timestamp, options...)
	if err != nil {
		return nil, err
	}
	return mcp.NewToolResultText(fmt.Sprintf("Message %s updated in channel %s", ts, channel)), nil
}

func (ch *ConversationsHandler) ConversationsDeleteMessageHandler(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	params, err := ch.parseParamsToolDeleteMessage(ctx, request)
	if err != nil {
		return nil, err
	}
	channel, ts, err := ch.apiProvider.Slack().DeleteMessageContext(ctx, params.channel, params.timestamp)
	if err != nil {
		return nil, err
	}
	return mcp.NewToolResultText(fmt.Sprintf("Message %s deleted from channel %s", ts, channel)), nil
}

func (ch *ConversationsHandler) FilesUploadHandler(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	params, err := ch.parseParamsToolUploadFile(ctx, request)
	if err != nil {
		return nil, err
	}

	data := []byte(params.content)
	if params.content64 != "" {
		data, err = base64.StdEncoding.DecodeString(params.content64)
		if err != nil {
			return nil, fmt.Errorf("content_base64 is not valid base64: %w", err)
		}
	}
	if len(data) == 0 {
		return nil, errors.New("provide either content or content_base64")
	}
	if len(data) > maxFileSizeBytes {
		return nil, fmt.Errorf("file size %d bytes exceeds maximum allowed size of %d bytes", len(data), maxFileSizeBytes)
	}

	file, err := ch.apiProvider.Slack().UploadFileContext(ctx, slack.UploadFileParameters{
		Filename:        params.filename,
		FileSize:        len(data),
		Content:         string(data),
		Title:           params.title,
		InitialComment:  params.comment,
		Channel:         params.channel,
		ThreadTimestamp: params.threadTs,
	})
	if err != nil {
		return nil, err
	}
	return mcp.NewToolResultText(fmt.Sprintf("Uploaded %s (%s, %d bytes) to channel %s", file.Title, file.ID, len(data), params.channel)), nil
}

func (ch *ConversationsHandler) FilesListHandler(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	params, err := ch.parseParamsToolFilesList(ctx, request)
	if err != nil {
		return nil, err
	}
	files, paging, err := ch.apiProvider.Slack().GetFilesContext(ctx, slack.GetFilesParameters{
		Channel: params.channel,
		Types:   params.types,
		Count:   params.limit,
		Page:    params.page,
	})
	if err != nil {
		return nil, err
	}
	rows := make([]FileRow, 0, len(files))
	for _, f := range files {
		rows = append(rows, FileRow{
			ID:        f.ID,
			Name:      f.Name,
			Title:     f.Title,
			FileType:  f.Filetype,
			Mimetype:  f.Mimetype,
			Size:      f.Size,
			UserID:    f.User,
			Permalink: f.Permalink,
		})
	}
	result, err := marshalRowsToCSV(rows)
	if err != nil {
		return nil, err
	}
	if paging != nil && paging.Pages > 0 && paging.Page < paging.Pages {
		result.Content = append(result.Content, mcp.TextContent{
			Type: "text",
			Text: fmt.Sprintf("\nNext page: %d", paging.Page+1),
		})
	}
	return result, nil
}

func (ch *ConversationsHandler) CanvasReadHandler(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	params, err := ch.parseParamsToolCanvasRead(request)
	if err != nil {
		return nil, err
	}
	fileInfo, _, _, err := ch.apiProvider.Slack().GetFileInfoContext(ctx, params.fileID, 0, 0)
	if err != nil {
		return nil, err
	}
	if fileInfo.Size > maxFileSizeBytes {
		return nil, fmt.Errorf("canvas size %d bytes exceeds maximum allowed size of %d bytes", fileInfo.Size, maxFileSizeBytes)
	}
	downloadURL := fileInfo.URLPrivateDownload
	if downloadURL == "" {
		downloadURL = fileInfo.URLPrivate
	}
	if downloadURL == "" {
		return nil, errors.New("canvas has no downloadable URL")
	}
	var buf bytes.Buffer
	if err := ch.apiProvider.Slack().GetFileContext(ctx, downloadURL, &buf); err != nil {
		return nil, err
	}

	content := buf.String()
	if fileInfo.Filetype == "quip" || fileInfo.Mimetype == "application/vnd.slack-docs" || strings.Contains(content, "quip-canvas-content") {
		content = text.HTMLToText(content)
	}
	result := map[string]any{
		"file_id":   fileInfo.ID,
		"title":     fileInfo.Title,
		"name":      fileInfo.Name,
		"filetype":  fileInfo.Filetype,
		"permalink": fileInfo.Permalink,
		"content":   content,
	}
	data, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	return mcp.NewToolResultText(string(data)), nil
}

func (ch *ConversationsHandler) ListsItemsListHandler(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	params, err := ch.parseParamsToolListsItems(request)
	if err != nil {
		return nil, err
	}
	resp, err := ch.apiProvider.Slack().ListsRecordsList(ctx, params.listID, params.limit, params.cursor)
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(map[string]any{
		"list_id":     params.listID,
		"records":     resp.Records,
		"list":        resp.List,
		"next_cursor": resp.ResponseMetadata.NextCursor,
	})
	if err != nil {
		return nil, err
	}
	return mcp.NewToolResultText(string(data)), nil
}

func (ch *ConversationsHandler) FilesGetHandler(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ch.logger.Debug("FilesGetHandler called", zap.Any("params", request.Params))

	if ready, err := ch.apiProvider.IsReady(); !ready {
		ch.logger.Error("API provider not ready", zap.Error(err))
		return nil, err
	}

	params, err := ch.parseParamsToolFilesGet(request)
	if err != nil {
		ch.logger.Error("Failed to parse attachment_get_data params", zap.Error(err))
		return nil, err
	}

	fileInfo, _, _, err := ch.apiProvider.Slack().GetFileInfoContext(ctx, params.fileID, 0, 0)
	if err != nil {
		ch.logger.Error("Slack GetFileInfoContext failed", zap.Error(err))
		return nil, err
	}

	if fileInfo.Size > maxFileSizeBytes {
		return nil, fmt.Errorf("file size %d bytes exceeds maximum allowed size of %d bytes", fileInfo.Size, maxFileSizeBytes)
	}

	var buf bytes.Buffer
	downloadURL := fileInfo.URLPrivateDownload
	if downloadURL == "" {
		downloadURL = fileInfo.URLPrivate
	}
	if downloadURL == "" {
		return nil, errors.New("file has no downloadable URL")
	}

	err = ch.apiProvider.Slack().GetFileContext(ctx, downloadURL, &buf)
	if err != nil {
		ch.logger.Error("Slack GetFileContext failed", zap.Error(err))
		return nil, err
	}

	content := buf.Bytes()

	// For image files, return as native MCP image content so the client
	// can render them directly without base64-in-JSON overflow.
	if isImageMimetype(fileInfo.Mimetype) {
		imageData := base64.StdEncoding.EncodeToString(content)
		metadata := fmt.Sprintf(`{"file_id":"%s","filename":"%s","mimetype":"%s","size":%d}`,
			fileInfo.ID,
			escapeJSON(fileInfo.Name),
			escapeJSON(fileInfo.Mimetype),
			len(content))
		return mcp.NewToolResultImage(metadata, imageData, fileInfo.Mimetype), nil
	}

	encoding := "none"
	var contentStr string

	if isTextMimetype(fileInfo.Mimetype) {
		contentStr = string(content)
	} else {
		contentStr = base64.StdEncoding.EncodeToString(content)
		encoding = "base64"
	}

	result := fmt.Sprintf(`{"file_id":"%s","filename":"%s","mimetype":"%s","size":%d,"encoding":"%s","content":"%s"}`,
		fileInfo.ID,
		escapeJSON(fileInfo.Name),
		escapeJSON(fileInfo.Mimetype),
		len(content),
		encoding,
		escapeJSON(contentStr))

	return mcp.NewToolResultText(result), nil
}

func isImageMimetype(mimetype string) bool {
	return strings.HasPrefix(mimetype, "image/")
}

func isTextMimetype(mimetype string) bool {
	if strings.HasPrefix(mimetype, "text/") {
		return true
	}
	textMimetypes := map[string]bool{
		"application/json":       true,
		"application/xml":        true,
		"application/javascript": true,
		"application/x-yaml":     true,
		"application/x-sh":       true,
	}
	return textMimetypes[mimetype]
}

func escapeJSON(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	s = strings.ReplaceAll(s, "\r", `\r`)
	s = strings.ReplaceAll(s, "\t", `\t`)
	return s
}

// ConversationsHistoryHandler streams conversation history as CSV. When
// thread_ts is supplied it returns that thread instead of the channel history.
func (ch *ConversationsHandler) ConversationsHistoryHandler(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ch.logger.Debug("ConversationsHistoryHandler called", zap.Any("params", request.Params))

	params, err := ch.parseParamsToolConversations(ctx, request)
	if err != nil {
		ch.logger.Error("Failed to parse history params", zap.Error(err))
		return nil, err
	}
	ch.logger.Debug("History params parsed",
		zap.String("channel", params.channel),
		zap.String("thread_ts", params.threadTs),
		zap.Int("limit", params.limit),
		zap.String("oldest", params.oldest),
		zap.String("latest", params.latest),
		zap.Bool("include_activity", params.activity),
	)

	if params.threadTs != "" {
		return ch.threadReplies(ctx, params)
	}

	historyParams := slack.GetConversationHistoryParameters{
		ChannelID: params.channel,
		Limit:     params.limit,
		Oldest:    params.oldest,
		Latest:    params.latest,
		Cursor:    params.cursor,
		Inclusive: false,
	}
	history, err := ch.apiProvider.Slack().GetConversationHistoryContext(ctx, &historyParams)
	if err != nil {
		ch.logger.Error("GetConversationHistoryContext failed", zap.Error(err))
		return nil, err
	}

	ch.logger.Debug("Fetched conversation history", zap.Int("message_count", len(history.Messages)))

	historyMessages := history.Messages
	if params.includeThreadReplies {
		historyMessages, err = ch.expandThreadReplies(ctx, historyMessages, params.channel)
		if err != nil {
			ch.logger.Error("Failed to expand thread replies", zap.Error(err))
			return nil, err
		}
	}

	messages := ch.convertMessagesFromHistory(ctx, historyMessages, params.channel, params.activity)

	if len(messages) > 0 && history.HasMore {
		messages[len(messages)-1].Cursor = history.ResponseMetaData.NextCursor
	}
	return marshalMessagesToCSV(messages)
}

// expandThreadReplies appends thread replies after each parent message. It is
// intentionally capped: a channel history request must not turn into an
// unbounded crawl.
func (ch *ConversationsHandler) expandThreadReplies(ctx context.Context, messages []slack.Message, channel string) ([]slack.Message, error) {
	lim := limiter.Tier3.Limiter()
	out := make([]slack.Message, 0, len(messages))
	for _, msg := range messages {
		out = append(out, msg)
		if msg.ReplyCount == 0 {
			continue
		}
		replies, err := ch.fetchAllThreadMessages(ctx, channel, msg.Timestamp)
		if err != nil {
			ch.logger.Warn("Failed to fetch thread replies; continuing with history",
				zap.String("channel", channel),
				zap.String("thread_ts", msg.Timestamp),
				zap.Error(err))
			continue
		}
		for i, reply := range replies {
			if i == 0 && reply.Timestamp == msg.Timestamp {
				continue
			}
			out = append(out, reply)
		}
		if len(out) >= maxExpandedHistoryMessages {
			ch.logger.Warn("Inline thread expansion capped",
				zap.String("channel", channel),
				zap.Int("messages", len(out)))
			break
		}
		if err := lim.Wait(ctx); err != nil {
			return out, err
		}
	}
	return out, nil
}

// ConversationsRepliesHandler streams thread replies as CSV
func (ch *ConversationsHandler) ConversationsRepliesHandler(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ch.logger.Debug("ConversationsRepliesHandler called", zap.Any("params", request.Params))

	params, err := ch.parseParamsToolConversations(ctx, request)
	if err != nil {
		ch.logger.Error("Failed to parse replies params", zap.Error(err))
		return nil, err
	}
	if params.threadTs == "" {
		ch.logger.Error("thread_ts not provided for replies")
		return nil, errors.New("thread_ts must be a timestamp or a full Slack message URL")
	}
	return ch.threadReplies(ctx, params)
}

// threadReplies fetches and serializes a thread. An optional query filters the
// returned messages by text after they are read from Slack.
func (ch *ConversationsHandler) threadReplies(ctx context.Context, params *conversationParams) (*mcp.CallToolResult, error) {
	replies, hasMore, nextCursor, err := ch.getConversationReplies(ctx, params)
	if err != nil {
		ch.logger.Error("GetConversationRepliesContext failed", zap.Error(err))
		return nil, err
	}
	ch.logger.Debug("Fetched conversation replies", zap.Int("count", len(replies)))

	messages := ch.convertMessagesFromHistory(ctx, replies, params.channel, params.activity)
	if params.query != "" {
		messages = filterMessagesByQuery(messages, params.query)
	}
	if len(messages) > 0 && hasMore {
		messages[len(messages)-1].Cursor = nextCursor
	}
	return marshalMessagesToCSV(messages)
}

// getConversationReplies reads a thread. If the supplied timestamp belongs to
// a reply rather than the thread parent, Slack returns only that one message;
// in that case retry with the parent timestamp so callers still get the whole
// thread.
func (ch *ConversationsHandler) getConversationReplies(ctx context.Context, params *conversationParams) ([]slack.Message, bool, string, error) {
	repliesParams := slack.GetConversationRepliesParameters{
		ChannelID: params.channel,
		Timestamp: params.threadTs,
		Limit:     params.limit,
		Oldest:    params.oldest,
		Latest:    params.latest,
		Cursor:    params.cursor,
		Inclusive: false,
	}
	replies, hasMore, nextCursor, err := ch.apiProvider.Slack().GetConversationRepliesContext(ctx, &repliesParams)
	if err != nil {
		return nil, false, "", err
	}
	if len(replies) == 1 && replies[0].ThreadTimestamp != "" && replies[0].ThreadTimestamp != params.threadTs {
		repliesParams.Timestamp = replies[0].ThreadTimestamp
		repliesParams.Cursor = ""
		return ch.apiProvider.Slack().GetConversationRepliesContext(ctx, &repliesParams)
	}
	return replies, hasMore, nextCursor, nil
}

func (ch *ConversationsHandler) ConversationsSearchHandler(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ch.logger.Debug("ConversationsSearchHandler called", zap.Any("params", request.Params))

	params, err := ch.parseParamsToolSearch(ctx, request)
	if err != nil {
		ch.logger.Error("Failed to parse search params", zap.Error(err))
		return nil, err
	}

	// A Slack message URL is resolved directly. This is the reliable way to
	// read a message that lives in a thread: Slack search can return the reply
	// but it drops the surrounding thread context.
	if params.messageRef != nil {
		return ch.searchThreadURL(ctx, params)
	}

	// Slack's search API cannot filter by thread, so when thread_ts is given
	// fetch the thread and filter it locally.
	if params.threadTs != "" {
		return ch.searchThread(ctx, params)
	}

	ch.logger.Debug("Search params parsed", zap.String("query", params.query), zap.Int("limit", params.limit), zap.Int("page", params.page))

	searchParams := slack.SearchParameters{
		Sort:          slack.DEFAULT_SEARCH_SORT,
		SortDirection: slack.DEFAULT_SEARCH_SORT_DIR,
		Highlight:     false,
		Count:         params.limit,
		Page:          params.page,
	}

	rl := limiter.Tier2.Limiter()
	messagesRes, err := limiter.CallWithRetry(ctx, rl, 2, slackRetryAfter, func() (*slack.SearchMessages, error) {
		msgs, _, err := ch.apiProvider.Slack().SearchContext(ctx, params.query, searchParams)
		return msgs, err
	})
	if err != nil {
		ch.logger.Error("Slack SearchContext failed", zap.Error(err))
		return nil, err
	}
	ch.logger.Debug("Search completed", zap.Int("matches", len(messagesRes.Matches)))

	if params.dateLocation != nil {
		// Slack applies date filters in the authenticated user's timezone. If
		// the caller asked for a different timezone, re-check the date locally.
		filtered := messagesRes.Matches[:0]
		for _, match := range messagesRes.Matches {
			if slackMessageMatchesDateFilters(match.Timestamp, params.filters, params.dateLocation) {
				filtered = append(filtered, match)
			}
		}
		messagesRes.Matches = filtered
	}

	messages := ch.convertMessagesFromSearch(ctx, messagesRes.Matches)
	if len(messages) > 0 && messagesRes.Pagination.Page < messagesRes.Pagination.PageCount {
		nextCursor := fmt.Sprintf("page:%d", messagesRes.Pagination.Page+1)
		messages[len(messages)-1].Cursor = encodeSearchCursor("page", nextCursor[len("page:"):])
	}
	return marshalMessagesToCSV(messages)
}

// searchThreadURL reads a thread referenced by a full Slack message URL.
func (ch *ConversationsHandler) searchThreadURL(ctx context.Context, params *searchParams) (*mcp.CallToolResult, error) {
	ref := params.messageRef
	cursor := ""
	if params.cursorMode == "thread" {
		cursor = params.cursor
	}
	cp := &conversationParams{
		channel:  ref.ChannelID,
		threadTs: ref.rootTS(),
		limit:    params.limit,
		cursor:   cursor,
	}
	messages, err := ch.threadMessages(ctx, cp)
	if err != nil {
		return nil, err
	}

	// threadMessages puts Slack's raw next cursor on the last row. Re-encode it
	// so a cursor produced by this tool can be decoded by parseParamsToolSearch.
	if len(messages) > 0 && messages[len(messages)-1].Cursor != "" {
		messages[len(messages)-1].Cursor = encodeSearchCursor("thread", messages[len(messages)-1].Cursor)
	}
	return marshalMessagesToCSV(messages)
}

// threadMessages fetches a thread without serializing it.
func (ch *ConversationsHandler) threadMessages(ctx context.Context, params *conversationParams) ([]Message, error) {
	replies, hasMore, nextCursor, err := ch.getConversationReplies(ctx, params)
	if err != nil {
		return nil, err
	}
	messages := ch.convertMessagesFromHistory(ctx, replies, params.channel, params.activity)
	if len(messages) > 0 && hasMore {
		messages[len(messages)-1].Cursor = nextCursor
	}
	return messages, nil
}

// searchThread searches inside one thread by reading its replies and applying
// the parsed query filters locally.
func (ch *ConversationsHandler) searchThread(ctx context.Context, params *searchParams) (*mcp.CallToolResult, error) {
	if params.threadChannel == "" {
		return nil, errors.New("filter_in_channel is required when thread_ts is set (or use a full Slack message URL)")
	}

	all, err := ch.fetchAllThreadMessages(ctx, params.threadChannel, params.threadTs)
	if err != nil {
		ch.logger.Error("Failed to fetch thread for search", zap.Error(err))
		return nil, err
	}

	matched := make([]slack.Message, 0, len(all))
	for _, msg := range all {
		if threadMessageMatches(msg, params.freeText, params.filters, params.dateLocation) {
			matched = append(matched, msg)
		}
	}
	ch.logger.Debug("Thread search completed",
		zap.String("channel", params.threadChannel),
		zap.String("thread_ts", params.threadTs),
		zap.Int("messages", len(all)),
		zap.Int("matches", len(matched)),
	)

	offset := 0
	if params.cursorMode == "tsearch" {
		if n, err := strconv.Atoi(params.cursor); err == nil && n > 0 {
			offset = n
		}
	}
	if offset > len(matched) {
		offset = len(matched)
	}
	end := offset + params.limit
	if end > len(matched) {
		end = len(matched)
	}

	messages := ch.convertMessagesFromHistory(ctx, matched[offset:end], params.threadChannel, false)
	if end < len(matched) && len(messages) > 0 {
		messages[len(messages)-1].Cursor = encodeSearchCursor("tsearch", strconv.Itoa(end))
	}
	return marshalMessagesToCSV(messages)
}

// fetchAllThreadMessages pages through a thread up to maxThreadSearchMessages.
func (ch *ConversationsHandler) fetchAllThreadMessages(ctx context.Context, channel, threadTs string) ([]slack.Message, error) {
	const repliesPageSize = 200
	lim := limiter.Tier3.Limiter()

	var all []slack.Message
	cursor := ""
	resolvedRoot := false
	for {
		params := slack.GetConversationRepliesParameters{
			ChannelID: channel,
			Timestamp: threadTs,
			Limit:     repliesPageSize,
			Cursor:    cursor,
			Inclusive: false,
		}
		replies, hasMore, nextCursor, err := ch.apiProvider.Slack().GetConversationRepliesContext(ctx, &params)
		if err != nil {
			return nil, err
		}
		if !resolvedRoot {
			resolvedRoot = true
			// A reply timestamp returns just that reply. Retry from its parent
			// so a search targeting a reply URL sees the whole thread.
			if len(replies) == 1 && replies[0].ThreadTimestamp != "" && replies[0].ThreadTimestamp != threadTs {
				threadTs = replies[0].ThreadTimestamp
				continue
			}
		}
		all = append(all, replies...)
		if !hasMore || nextCursor == "" || nextCursor == cursor {
			break
		}
		if len(all) >= maxThreadSearchMessages {
			ch.logger.Warn("Thread search capped; returning a partial thread",
				zap.String("channel", channel),
				zap.String("thread_ts", threadTs),
				zap.Int("messages", len(all)))
			break
		}
		cursor = nextCursor
		if err := lim.Wait(ctx); err != nil {
			return nil, err
		}
	}
	if len(all) > maxThreadSearchMessages {
		all = all[:maxThreadSearchMessages]
	}
	return all, nil
}

func encodeSearchCursor(mode, value string) string {
	return base64.StdEncoding.EncodeToString([]byte(mode + ":" + value))
}

// UnreadChannel represents a channel with unread messages
type UnreadChannel struct {
	ChannelID   string `json:"channelID"`
	ChannelName string `json:"channelName"`
	ChannelType string `json:"channelType"` // "dm", "group_dm", "partner", "internal"
	UnreadCount int    `json:"unreadCount"`
	LastRead    string `json:"lastRead"`
	Latest      string `json:"latest"`
}

// UnreadMessage extends Message with channel context
type UnreadMessage struct {
	Message
	ChannelType string `json:"channelType"`
}

// ConversationsUnreadsHandler returns unread messages across all channels
func (ch *ConversationsHandler) ConversationsUnreadsHandler(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ch.logger.Debug("ConversationsUnreadsHandler called", zap.Any("params", request.Params))

	params := ch.parseParamsToolUnreads(request)

	// Fetch muted channels unless the caller wants them included
	if !params.includeMuted {
		mutedChannels, err := ch.apiProvider.Slack().GetMutedChannels(ctx)
		if err != nil {
			ch.logger.Warn("Failed to fetch muted channels, proceeding without mute filter", zap.Error(err))
			params.mutedUnavailable = true
		} else if len(mutedChannels) > 0 {
			params.mutedChannels = mutedChannels
			ch.logger.Debug("Loaded muted channels", zap.Int("count", len(mutedChannels)))
		}
	}

	// Route based on token type:
	// - xoxc/xoxd (browser session): use fast client.counts API
	// - xoxp (OAuth user): fall back to conversations.info/history approach
	// - xoxb (bot): not supported — unreads is a user-level concept
	if ch.apiProvider.IsOAuth() {
		if ch.apiProvider.IsBotToken() {
			return nil, fmt.Errorf(
				"conversations_unreads requires a user token (xoxp) or browser session tokens (xoxc/xoxd); " +
					"bot tokens (xoxb) do not support unread tracking",
			)
		}
		ch.logger.Info("OAuth token detected, using conversations.info fallback for unreads")
		return ch.getUnreadsViaConversationsInfo(ctx, params)
	}

	counts, err := ch.apiProvider.Slack().ClientCounts(ctx)
	if err != nil {
		ch.logger.Error("ClientCounts failed", zap.Error(err))
		return nil, fmt.Errorf("failed to get client counts: %v", err)
	}

	return ch.processClientCountsResponse(ctx, params, counts)
}

func (ch *ConversationsHandler) processClientCountsResponse(ctx context.Context, params *unreadsParams, counts edge.ClientCountsResponse) (*mcp.CallToolResult, error) {
	ch.logger.Debug("Got counts data",
		zap.Int("channels", len(counts.Channels)),
		zap.Int("mpims", len(counts.MPIMs)),
		zap.Int("ims", len(counts.IMs)))

	// Get users map and channels map for resolving names
	usersMap := ch.apiProvider.ProvideUsersMap()
	channelsMaps := ch.apiProvider.ProvideChannelsMaps()

	// Collect channels with unreads
	var unreadChannels []UnreadChannel

	// Process regular channels (public, private)
	for _, snap := range counts.Channels {
		if !snap.HasUnreads {
			continue
		}

		// Skip muted channels (unless include_muted is set)
		if params.mutedChannels[snap.ID] {
			continue
		}

		// Priority Inbox: skip channels without @mentions
		if params.mentionsOnly && snap.MentionCount == 0 {
			continue
		}

		// Get channel info from cache to determine type and name
		channelName := snap.ID
		channelType := "internal"
		if cached, ok := channelsMaps.Channels[snap.ID]; ok {
			// The cached name may already have # prefix, so handle both cases
			name := cached.Name
			if strings.HasPrefix(name, "#") {
				channelName = name
			} else {
				channelName = "#" + name
			}
			// Check if it's a partner/external channel using Slack's metadata
			if cached.IsExtShared {
				channelType = "partner"
			}
		}

		// Filter by requested channel types
		if params.channelTypes != "all" && channelType != params.channelTypes {
			continue
		}

		unreadChannels = append(unreadChannels, UnreadChannel{
			ChannelID:   snap.ID,
			ChannelName: channelName,
			ChannelType: channelType,
			UnreadCount: snap.MentionCount,
			LastRead:    snap.LastRead.SlackString(),
			Latest:      snap.Latest.SlackString(),
		})
	}

	// Process MPIMs (group DMs)
	for _, snap := range counts.MPIMs {
		if !snap.HasUnreads {
			continue
		}

		// Skip muted channels (unless include_muted is set)
		if params.mutedChannels[snap.ID] {
			continue
		}

		// Priority Inbox: skip channels without @mentions
		if params.mentionsOnly && snap.MentionCount == 0 {
			continue
		}

		// Filter by requested channel types
		if params.channelTypes != "all" && params.channelTypes != "group_dm" {
			continue
		}

		channelName := snap.ID
		if cached, ok := channelsMaps.Channels[snap.ID]; ok {
			channelName = cached.Name
		}

		unreadChannels = append(unreadChannels, UnreadChannel{
			ChannelID:   snap.ID,
			ChannelName: channelName,
			ChannelType: "group_dm",
			UnreadCount: snap.MentionCount,
			LastRead:    snap.LastRead.SlackString(),
			Latest:      snap.Latest.SlackString(),
		})
	}

	// Process IMs (direct messages)
	for _, snap := range counts.IMs {
		if !snap.HasUnreads {
			continue
		}

		// Skip muted channels (unless include_muted is set)
		if params.mutedChannels[snap.ID] {
			continue
		}

		// Priority Inbox: skip channels without @mentions
		if params.mentionsOnly && snap.MentionCount == 0 {
			continue
		}

		// Filter by requested channel types
		if params.channelTypes != "all" && params.channelTypes != "dm" {
			continue
		}

		// Get display name for DM from channel cache or users
		channelName := snap.ID
		if cached, ok := channelsMaps.Channels[snap.ID]; ok {
			if cached.User != "" {
				if u, ok := usersMap.Users[cached.User]; ok {
					channelName = "@" + u.Name
				} else {
					channelName = "@" + cached.User
				}
			}
		}

		unreadChannels = append(unreadChannels, UnreadChannel{
			ChannelID:   snap.ID,
			ChannelName: channelName,
			ChannelType: "dm",
			UnreadCount: snap.MentionCount,
			LastRead:    snap.LastRead.SlackString(),
			Latest:      snap.Latest.SlackString(),
		})
	}

	// Sort by priority: DMs > partner channels > internal
	ch.sortChannelsByPriority(unreadChannels)

	// Limit channels
	if len(unreadChannels) > params.maxChannels {
		unreadChannels = unreadChannels[:params.maxChannels]
	}

	ch.logger.Debug("Found unread channels", zap.Int("count", len(unreadChannels)))

	// Backfill real unread counts for channels where client.counts only gave us
	// HasUnreads=true but MentionCount=0 (unreads without @mentions).
	// DMs and group DMs don't need this — every DM message counts as a mention.
	//
	// NOTE: conversations.info does not return unread_count with browser tokens
	// (xoxc/xoxd), so we use conversations.history to count messages since the
	// last-read timestamp. Limit kept small (20) for speed; the exact count
	// matters less than surfacing that unreads exist.
	const backfillLimit = 20
	backfilled := 0
	for i := range unreadChannels {
		if unreadChannels[i].UnreadCount > 0 {
			continue // MentionCount was positive, good enough
		}
		if unreadChannels[i].LastRead == "" {
			// No last-read timestamp means we can't bound the query.
			// Conservatively report 1 unread since HasUnreads was true.
			unreadChannels[i].UnreadCount = 1
			backfilled++
			continue
		}
		history, err := ch.apiProvider.Slack().GetConversationHistoryContext(ctx,
			&slack.GetConversationHistoryParameters{
				ChannelID: unreadChannels[i].ChannelID,
				Oldest:    unreadChannels[i].LastRead,
				Limit:     backfillLimit,
				Inclusive: false,
			})
		if err != nil {
			ch.logger.Debug("Failed to backfill unread count",
				zap.String("channel", unreadChannels[i].ChannelID),
				zap.Error(err))
			continue
		}
		if len(history.Messages) > 0 {
			unreadChannels[i].UnreadCount = len(history.Messages)
		}
		backfilled++
	}
	if backfilled > 0 {
		ch.logger.Debug("Backfilled unread counts via conversations.history",
			zap.Int("backfilled", backfilled))
	}

	// If not including messages, just return channel summary
	if !params.includeMessages {
		return ch.marshalUnreadChannelsToCSV(unreadChannels)
	}

	// Fetch messages for each unread channel
	var allMessages []Message

	for i := range unreadChannels {
		historyParams := slack.GetConversationHistoryParameters{
			ChannelID: unreadChannels[i].ChannelID,
			Oldest:    unreadChannels[i].LastRead,
			Limit:     params.maxMessagesPerChannel,
			Inclusive: false,
		}

		history, err := ch.apiProvider.Slack().GetConversationHistoryContext(ctx, &historyParams)
		if err != nil {
			ch.logger.Warn("Failed to get history for channel",
				zap.String("channel", unreadChannels[i].ChannelID),
				zap.Error(err))
			continue
		}

		// Update unread count from actual message count
		unreadChannels[i].UnreadCount = len(history.Messages)

		// Convert messages
		channelMessages := ch.convertMessagesFromHistory(ctx, history.Messages, unreadChannels[i].ChannelName, false)
		allMessages = append(allMessages, channelMessages...)
	}

	ch.logger.Debug("Fetched unread messages", zap.Int("total", len(allMessages)))

	return marshalMessagesToCSV(allMessages)
}

func (ch *ConversationsHandler) getUnreadsViaConversationsInfo(ctx context.Context, params *unreadsParams) (*mcp.CallToolResult, error) {
	usersMap := ch.apiProvider.ProvideUsersMap()

	// Define channel type groups in priority order.
	// users.conversations returns channels in creation order (NOT by activity),
	// so we scan each type independently with its own budget/scan cap.
	type typeGroup struct {
		slackTypes  []string // users.conversations "types" parameter values
		channelType string   // our internal type label
		budget      int      // max unread channels to return for this group
		isDM        bool     // DMs get unread_count directly from conversations.info
	}

	// Allocate budgets: DMs get the full max_channels allocation,
	// MPIMs and channels each get half. Each type group scans up to
	// budget*2 channels to find unreads (see scanTypeGroupForUnreads).
	dmBudget := params.maxChannels
	mpimBudget := params.maxChannels / 2
	channelBudget := params.maxChannels / 2

	groups := []typeGroup{
		{slackTypes: []string{"im"}, channelType: "dm", budget: dmBudget, isDM: true},
		{slackTypes: []string{"mpim"}, channelType: "group_dm", budget: mpimBudget, isDM: false},
		{slackTypes: []string{"public_channel", "private_channel"}, channelType: "", budget: channelBudget, isDM: false},
	}

	var unreadChannels []UnreadChannel
	totalAPIcalls := 0
	totalScanned := 0
	totalRateLimited := 0

	for _, group := range groups {
		// Apply channel_types filter: skip groups that don't match the requested filter.
		if params.channelTypes != "all" {
			match := false
			switch params.channelTypes {
			case "dm":
				match = group.channelType == "dm"
			case "group_dm":
				match = group.channelType == "group_dm"
			case "internal", "partner":
				// Both internal and partner channels come from public/private channel types
				match = group.channelType == ""
			}
			if !match {
				continue
			}
		}

		found, apiCalls, scanned, rateLimited := ch.scanTypeGroupForUnreads(ctx, params, usersMap, group.slackTypes, group.channelType, group.budget, group.isDM)
		unreadChannels = append(unreadChannels, found...)
		totalAPIcalls += apiCalls
		totalScanned += scanned
		totalRateLimited += rateLimited
	}

	ch.sortChannelsByPriority(unreadChannels)

	ch.logger.Info("Found unread channels via xoxp fallback",
		zap.Int("count", len(unreadChannels)),
		zap.Int("scanned", totalScanned),
		zap.Int("apiCalls", totalAPIcalls),
		zap.Int("rateLimited", totalRateLimited))

	// Prepend a note about xoxp limitations so the LLM understands
	// these results may be partial.
	mutedNote := ""
	if params.mutedUnavailable && !params.includeMuted {
		mutedNote = "Muted channel filtering is unavailable with xoxp tokens; results may include muted channels. "
	}
	rateLimitNote := ""
	if totalRateLimited > 0 {
		rateLimitNote = fmt.Sprintf("WARNING: %d channels were skipped due to Slack rate limiting (even after retries) — results are degraded. Try again after a brief cooldown. ", totalRateLimited)
	}
	xoxpNote := fmt.Sprintf(
		"[xoxp token: scanned %d channels (%d API calls), found %d with unreads. %s%s"+
			"Results may be incomplete — increase max_channels for broader coverage, "+
			"or use xoxc/xoxd browser tokens for complete results.]\n\n",
		totalScanned, totalAPIcalls, len(unreadChannels), rateLimitNote, mutedNote,
	)

	if !params.includeMessages {
		result, err := ch.marshalUnreadChannelsToCSV(unreadChannels)
		if err != nil {
			return nil, err
		}
		// Prepend the xoxp note to the CSV output
		if len(result.Content) > 0 {
			if tc, ok := result.Content[0].(mcp.TextContent); ok {
				tc.Text = xoxpNote + tc.Text
				result.Content[0] = tc
			}
		}
		return result, nil
	}

	// Fetch actual unread messages for each discovered channel
	rl := limiter.Tier3.Limiter()
	var allMessages []Message
	for _, uc := range unreadChannels {
		historyParams := slack.GetConversationHistoryParameters{
			ChannelID: uc.ChannelID,
			Oldest:    uc.LastRead,
			Limit:     params.maxMessagesPerChannel,
			Inclusive: false,
		}

		history, err := limiter.CallWithRetry(ctx, rl, 2, slackRetryAfter, func() (*slack.GetConversationHistoryResponse, error) {
			return ch.apiProvider.Slack().GetConversationHistoryContext(ctx, &historyParams)
		})
		if err != nil {
			ch.logger.Warn("Failed to get history for channel",
				zap.String("channel", uc.ChannelID),
				zap.Error(err))
			continue
		}

		channelMessages := ch.convertMessagesFromHistory(ctx, history.Messages, uc.ChannelName, false)
		allMessages = append(allMessages, channelMessages...)
	}

	ch.logger.Debug("Fetched unread messages via fallback", zap.Int("total", len(allMessages)))

	result, err := marshalMessagesToCSV(allMessages)
	if err != nil {
		return nil, err
	}
	// Prepend the xoxp note to the messages CSV output
	if len(result.Content) > 0 {
		if tc, ok := result.Content[0].(mcp.TextContent); ok {
			tc.Text = xoxpNote + tc.Text
			result.Content[0] = tc
		}
	}
	return result, nil
}

// slackRetryAfter checks if an error is a Slack rate limit error and returns
// the retry-after duration. Returns 0 for non-rate-limit errors.
// Used as the retryAfter callback for limiter.CallWithRetry.
func slackRetryAfter(err error) time.Duration {
	var rle *slack.RateLimitedError
	if errors.As(err, &rle) {
		return rle.RetryAfter
	}
	return 0
}

// scanTypeGroupForUnreads fetches channels of the given Slack types via users.conversations
// and checks each for unreads via conversations.info.
//
// users.conversations returns only channels the calling user is a member of, which is
// significantly more efficient than conversations.list (excludes non-member public channels
// and closed DMs that cannot have unreads).
//
// Neither endpoint sorts by activity — channels are returned in creation order.
// Unread channels can appear anywhere in the list, so we scan up to a capped number
// per type group to keep API call count bounded (each channel costs 1-2 API calls).
//
// Returns the discovered unread channels, total API calls made, channels scanned,
// and the number of channels skipped due to rate limiting.
func (ch *ConversationsHandler) scanTypeGroupForUnreads(
	ctx context.Context,
	params *unreadsParams,
	usersMap *provider.UsersCache,
	slackTypes []string,
	groupType string,
	budget int,
	isDM bool,
) ([]UnreadChannel, int, int, int) {
	var unreadChannels []UnreadChannel
	apiCalls := 0
	scanned := 0
	rateLimited := 0

	// Proactive rate limiter for conversations.info (Tier 3: ~50 req/min).
	// Without this, the scan fires requests as fast as the network allows,
	// triggering cascading 429s that silently skip channels.
	rl := limiter.Tier3.Limiter()

	// users.conversations returns channels in creation order (not by activity),
	// so unread channels can appear anywhere. We scan up to budget*2 channels
	// per type group — a trade-off between coverage and API call count.
	// Each channel costs 1 API call (DMs: conversations.info returns
	// unread_count directly) or up to 2 calls (non-DMs: conversations.info
	// for last_read + conversations.history to detect new messages).
	//
	// With the default max_channels=50, this gives:
	//   DMs:      scan up to 100 channels (~100 API calls)
	//   MPIMs:    scan up to  50 channels (~100 API calls)
	//   Channels: scan up to  50 channels (~100 API calls)
	//   Total:    ~300 API calls max (vs ~2000+ unbounded)
	maxScan := budget * 2
	if maxScan < 50 {
		maxScan = 50
	}

	cursor := ""
	for {
		if len(unreadChannels) >= budget {
			break
		}
		if maxScan > 0 && scanned >= maxScan {
			break
		}

		// Fetch a page of channels via users.conversations (only channels the
		// calling user is a member of). This is significantly more efficient than
		// conversations.list for xoxp tokens because it excludes non-member public
		// channels and closed DMs — channels that cannot have unreads.
		// Empirically tested: 37% fewer channels on a 2700-channel workspace
		// (1724 vs 2737), with 85% reduction for public channels (156 vs 1046).
		// Both methods miss the same set of archived channels, so zero real
		// unreads are lost by this switch.
		userConvParams := &slack.GetConversationsForUserParameters{
			Types:           slackTypes,
			Limit:           200,
			ExcludeArchived: true,
			Cursor:          cursor,
		}

		channels, nextCursor, err := ch.apiProvider.Slack().GetConversationsForUserContext(ctx, userConvParams)
		apiCalls++
		if err != nil {
			ch.logger.Warn("Failed to list conversations for type group",
				zap.Strings("types", slackTypes),
				zap.Error(err))
			break
		}

		if len(channels) == 0 {
			break
		}

		for _, channel := range channels {
			if len(unreadChannels) >= budget {
				break
			}
			if maxScan > 0 && scanned >= maxScan {
				break
			}

			// Skip muted channels
			if params.mutedChannels[channel.ID] {
				continue
			}

			scanned++

			// Get full channel info including last_read and latest.
			// Uses rate limiting + retry to avoid cascading 429 errors
			// that silently skip channels (see: slack-go does NOT auto-retry
			// on *RateLimitedError for standard client methods).
			info, err := limiter.CallWithRetry(ctx, rl, 2, slackRetryAfter, func() (*slack.Channel, error) {
				return ch.apiProvider.Slack().GetConversationInfoContext(ctx, &slack.GetConversationInfoInput{
					ChannelID: channel.ID,
				})
			})
			apiCalls++
			if err != nil {
				var rle *slack.RateLimitedError
				if errors.As(err, &rle) {
					rateLimited++
					ch.logger.Warn("Rate limited on conversation info (retries exhausted)",
						zap.String("channel", channel.ID),
						zap.Duration("retryAfter", rle.RetryAfter))
				} else {
					ch.logger.Debug("Failed to get conversation info",
						zap.String("channel", channel.ID),
						zap.Error(err))
				}
				continue
			}

			// Determine if channel has unreads
			hasUnreads := false
			unreadCount := 0

			if isDM {
				// DMs: conversations.info returns unread_count directly
				if info.UnreadCount > 0 {
					hasUnreads = true
					unreadCount = info.UnreadCount
				}
			} else {
				// Channels/groups/MPIMs: conversations.info does NOT return
				// unread_count or latest message for xoxp tokens. Use last_read
				// with conversations.history to detect unreads.
				//
				// last_read values:
				//   ""                    → never visited (skip — noise on large workspaces)
				//   "0000000000.000000"   → never read (Slack sentinel, treat as "never visited")
				//   "<timestamp>"         → last read position
				lastRead := info.LastRead
				neverVisited := lastRead == "" || lastRead == "0000000000.000000"

				if neverVisited && groupType != "group_dm" {
					// For regular channels: skip if never visited/read. On large
					// workspaces this includes hundreds of auto-joined or dormant
					// channels — skip to avoid flooding results with stale unreads.
					// MPIMs are always intentional (someone added you), so check them.
					continue
				}

				if neverVisited {
					lastRead = "0" // normalize sentinel to valid oldest param
				}

				historyParams := slack.GetConversationHistoryParameters{
					ChannelID: channel.ID,
					Oldest:    lastRead,
					Limit:     params.maxMessagesPerChannel,
					Inclusive: false,
				}
				history, err := limiter.CallWithRetry(ctx, rl, 2, slackRetryAfter, func() (*slack.GetConversationHistoryResponse, error) {
					return ch.apiProvider.Slack().GetConversationHistoryContext(ctx, &historyParams)
				})
				apiCalls++
				if err != nil {
					var rle *slack.RateLimitedError
					if errors.As(err, &rle) {
						rateLimited++
						ch.logger.Warn("Rate limited on conversation history (retries exhausted)",
							zap.String("channel", channel.ID),
							zap.Duration("retryAfter", rle.RetryAfter))
					} else {
						ch.logger.Debug("Failed to get history for unread check",
							zap.String("channel", channel.ID),
							zap.Error(err))
					}
				} else if len(history.Messages) > 0 {
					hasUnreads = true
					unreadCount = len(history.Messages)
				}
			}

			if !hasUnreads {
				continue
			}

			// Determine channel type and display name
			channelType := groupType
			if channelType == "" {
				// For public/private channels, categorize as internal or partner
				if info.IsExtShared {
					channelType = "partner"
				} else {
					channelType = "internal"
				}
			}

			// Apply channel_types filter for internal vs partner
			if params.channelTypes != "all" && params.channelTypes != channelType {
				continue
			}

			channelName := ch.getChannelDisplayName(info, channelType, usersMap)

			latestTs := ""
			if info.Latest != nil {
				latestTs = info.Latest.Timestamp
			}

			unreadChannels = append(unreadChannels, UnreadChannel{
				ChannelID:   channel.ID,
				ChannelName: channelName,
				ChannelType: channelType,
				UnreadCount: unreadCount,
				LastRead:    info.LastRead,
				Latest:      latestTs,
			})
		}

		// Move to next page if available
		if nextCursor == "" {
			break
		}
		cursor = nextCursor
	}

	ch.logger.Debug("Scanned type group",
		zap.Strings("types", slackTypes),
		zap.Int("scanned", scanned),
		zap.Int("found", len(unreadChannels)),
		zap.Int("apiCalls", apiCalls),
		zap.Int("rateLimited", rateLimited))

	return unreadChannels, apiCalls, scanned, rateLimited
}

// getChannelDisplayName returns a human-readable name for a channel from conversations.info data.
func (ch *ConversationsHandler) getChannelDisplayName(info *slack.Channel, channelType string, usersMap *provider.UsersCache) string {
	switch channelType {
	case "dm":
		if info.User != "" {
			if u, ok := usersMap.Users[info.User]; ok {
				return "@" + u.Name
			}
			return "@" + info.User
		}
		return info.ID
	case "group_dm":
		return info.Name
	default:
		name := info.Name
		if !strings.HasPrefix(name, "#") {
			return "#" + name
		}
		return name
	}
}

// ConversationsMarkHandler marks a channel as read up to a specific timestamp
func (ch *ConversationsHandler) ConversationsMarkHandler(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ch.logger.Debug("ConversationsMarkHandler called", zap.Any("params", request.Params))

	params, err := ch.parseParamsToolMark(request)
	if err != nil {
		ch.logger.Error("Failed to parse mark params", zap.Error(err))
		return nil, err
	}

	channel := params.channel
	ts := params.ts

	if ts == "" {
		// Fetch the latest message to get its timestamp
		historyParams := slack.GetConversationHistoryParameters{
			ChannelID: channel,
			Limit:     1,
		}
		history, err := ch.apiProvider.Slack().GetConversationHistoryContext(ctx, &historyParams)
		if err != nil {
			ch.logger.Error("Failed to get latest message", zap.Error(err))
			return nil, fmt.Errorf("failed to get latest message: %v", err)
		}
		if len(history.Messages) > 0 {
			ts = history.Messages[0].Timestamp
		} else {
			// No messages in channel, nothing to mark
			return mcp.NewToolResultText("No messages to mark as read"), nil
		}
	}

	// Mark the conversation as read
	err = ch.apiProvider.Slack().MarkConversationContext(ctx, channel, ts)
	if err != nil {
		ch.logger.Error("Failed to mark conversation", zap.Error(err))
		return nil, fmt.Errorf("failed to mark conversation as read: %v", err)
	}

	ch.logger.Info("Marked conversation as read",
		zap.String("channel", channel),
		zap.String("ts", ts))

	return mcp.NewToolResultText(fmt.Sprintf("Marked %s as read up to %s", channel, ts)), nil
}

func (ch *ConversationsHandler) ConversationsLeaveHandler(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ch.logger.Debug("ConversationsLeaveHandler called", zap.Any("params", request.Params))

	channel := request.GetString("channel_id", "")
	if channel == "" {
		return nil, fmt.Errorf("channel_id is required")
	}

	channel, err := ch.resolveChannelID(ctx, channel)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve channel: %w", err)
	}

	notInChannel, err := ch.apiProvider.Slack().LeaveConversationContext(ctx, channel)
	if err != nil {
		ch.logger.Error("Failed to leave conversation", zap.Error(err))
		return nil, fmt.Errorf("failed to leave conversation: %v", err)
	}

	if notInChannel {
		ch.logger.Info("Was not in channel", zap.String("channel", channel))
		return mcp.NewToolResultText(fmt.Sprintf("Not a member of %s", channel)), nil
	}

	ch.logger.Info("Left conversation", zap.String("channel", channel))
	return mcp.NewToolResultText(fmt.Sprintf("Successfully left %s", channel)), nil
}

func (ch *ConversationsHandler) ConversationsJoinHandler(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ch.logger.Debug("ConversationsJoinHandler called", zap.Any("params", request.Params))

	channel := request.GetString("channel_id", "")
	if channel == "" {
		return nil, fmt.Errorf("channel_id is required")
	}

	channel, err := ch.resolveChannelID(ctx, channel)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve channel: %w", err)
	}

	_, _, _, err = ch.apiProvider.Slack().JoinConversationContext(ctx, channel)
	if err != nil {
		ch.logger.Error("Failed to join conversation", zap.Error(err))
		return nil, fmt.Errorf("failed to join conversation: %v", err)
	}

	ch.logger.Info("Joined conversation", zap.String("channel", channel))
	return mcp.NewToolResultText(fmt.Sprintf("Successfully joined %s", channel)), nil
}

// sortChannelsByPriority sorts channels: DMs > group_dm > partner > internal
func (ch *ConversationsHandler) sortChannelsByPriority(channels []UnreadChannel) {
	priority := map[string]int{
		"dm":       0,
		"group_dm": 1,
		"partner":  2,
		"internal": 3,
	}

	sort.Slice(channels, func(i, j int) bool {
		pi := priority[channels[i].ChannelType]
		pj := priority[channels[j].ChannelType]
		return pi < pj
	})
}

// marshalUnreadChannelsToCSV converts unread channels to CSV format
func (ch *ConversationsHandler) marshalUnreadChannelsToCSV(channels []UnreadChannel) (*mcp.CallToolResult, error) {
	csvBytes, err := gocsv.MarshalBytes(&channels)
	if err != nil {
		return nil, err
	}
	return mcp.NewToolResultText(string(csvBytes)), nil
}

func isChannelAllowedForConfig(channel, config string) bool {
	if config == "" || config == "true" || config == "1" {
		return true
	}
	items := strings.Split(config, ",")
	isNegated := strings.HasPrefix(strings.TrimSpace(items[0]), "!")
	for _, item := range items {
		item = strings.TrimSpace(item)
		if isNegated {
			if strings.TrimPrefix(item, "!") == channel {
				return false
			}
		} else {
			if item == channel {
				return true
			}
		}
	}
	return isNegated
}

func isChannelAllowed(channel string) bool {
	return isChannelAllowedForConfig(channel, os.Getenv("SLACK_MCP_ADD_MESSAGE_TOOL"))
}

func (ch *ConversationsHandler) resolveChannelID(ctx context.Context, channel string) (string, error) {
	if !strings.HasPrefix(channel, "#") && !strings.HasPrefix(channel, "@") {
		return channel, nil
	}

	// First attempt: try to resolve from current cache
	channelsMaps := ch.apiProvider.ProvideChannelsMaps()
	chn, ok := channelsMaps.ChannelsInv[channel]
	if ok {
		return channelsMaps.Channels[chn].ID, nil
	}

	// Channel not found - try refreshing cache and retry once
	ch.logger.Debug("Channel not found in cache, attempting refresh",
		zap.String("channel", channel))

	refreshErr := ch.apiProvider.ForceRefreshChannels(ctx)
	wasRateLimited := errors.Is(refreshErr, provider.ErrRefreshRateLimited)

	if refreshErr != nil && !wasRateLimited {
		ch.logger.Error("Failed to refresh channels cache",
			zap.String("channel", channel),
			zap.Error(refreshErr))
		return "", fmt.Errorf("channel %q not found and cache refresh failed: %w", channel, refreshErr)
	}

	// If rate-limited, cache wasn't refreshed - no point in a second lookup
	if wasRateLimited {
		ch.logger.Warn("Channel not found; cache refresh was rate-limited",
			zap.String("channel", channel))
		return "", fmt.Errorf("channel %q not found (cache refresh was rate-limited, try again later)", channel)
	}

	// Second attempt after successful refresh
	channelsMaps = ch.apiProvider.ProvideChannelsMaps()
	chn, ok = channelsMaps.ChannelsInv[channel]
	if !ok {
		ch.logger.Error("Channel not found even after cache refresh",
			zap.String("channel", channel))
		return "", fmt.Errorf("channel %q not found", channel)
	}

	ch.logger.Debug("Channel found after cache refresh",
		zap.String("channel", channel),
		zap.String("channel_id", channelsMaps.Channels[chn].ID))

	return channelsMaps.Channels[chn].ID, nil
}

func (ch *ConversationsHandler) convertMessagesFromHistory(ctx context.Context, slackMessages []slack.Message, channel string, includeActivity bool) []Message {
	resolver := ch.newUserResolver(ctx)
	baseURL := ch.workspaceBaseURL()
	var messages []Message
	warn := false

	for _, msg := range slackMessages {
		if (msg.SubType != "" && msg.SubType != "bot_message" && msg.SubType != "thread_broadcast") && !includeActivity {
			continue
		}

		userName, realName, ok := resolver.resolve(msg.User)

		if !ok && msg.SubType == "bot_message" {
			userName, realName, ok = getBotInfo(msg.Username)
		}

		if !ok {
			warn = true
		}

		timestamp, err := text.TimestampToIsoRFC3339(msg.Timestamp)
		if err != nil {
			ch.logger.Error("Failed to convert timestamp to RFC3339", zap.Error(err))
			continue
		}

		msgText := msg.Text
		if msgText == "" {
			msgText = text.BlocksToText(msg.Blocks)
		}
		if msgText == "" {
			msgText = text.FilesToText(msg.Files)
		}
		msgText += text.AttachmentsTo2CSV(msgText, msg.Attachments)

		var reactionParts []string
		for _, r := range msg.Reactions {
			reactionParts = append(reactionParts, fmt.Sprintf("%s:%d", r.Name, r.Count))
		}
		reactionsString := strings.Join(reactionParts, "|")
		reactionUsersString := formatReactionUsers(msg.Reactions, resolver)

		botName := ""
		if msg.BotProfile != nil && msg.BotProfile.Name != "" {
			botName = msg.BotProfile.Name
		}

		fileCount := len(msg.Files)
		hasMedia := fileCount > 0 || hasImageBlocks(msg.Blocks)

		var attachmentIDs []string
		for _, f := range msg.Files {
			if f.Name != "" {
				attachmentIDs = append(attachmentIDs, fmt.Sprintf("%s (%s)", f.ID, f.Name))
			} else {
				attachmentIDs = append(attachmentIDs, f.ID)
			}
		}
		attachmentIDsStr := strings.Join(attachmentIDs, ", ")

		threadTs := msg.ThreadTimestamp
		if threadTs == "" && msg.ReplyCount > 0 {
			// conversations.history does not set thread_ts on a parent message.
			// Surface the parent timestamp so the caller can fetch its replies.
			threadTs = msg.Timestamp
		}

		messages = append(messages, Message{
			MsgID:         msg.Timestamp,
			UserID:        msg.User,
			UserName:      userName,
			RealName:      realName,
			Text:          text.ProcessText(msgText),
			Channel:       channel,
			ThreadTs:      threadTs,
			Time:          timestamp,
			Permalink:     slackPermalink(baseURL, channel, msg.Timestamp, threadTs),
			Reactions:     reactionsString,
			ReactionUsers: reactionUsersString,
			BotName:       botName,
			FileCount:     fileCount,
			AttachmentIDs: attachmentIDsStr,
			HasMedia:      hasMedia,
			ReplyCount:    msg.ReplyCount,
			ReplyUsers:    len(msg.ReplyUsers),
			LatestReply:   msg.LatestReply,
		})
	}

	if ready, err := ch.apiProvider.IsReady(); !ready {
		if warn && errors.Is(err, provider.ErrUsersNotReady) {
			ch.logger.Warn(
				"WARNING: Slack users sync is not ready yet, you may experience some limited functionality and see UIDs instead of resolved names as well as unable to query users by their @handles. Users sync is part of channels sync and operations on channels depend on users collection (IM, MPIM). Please wait until users are synced and try again",
				zap.Error(err),
			)
		}
	}
	return messages
}

func (ch *ConversationsHandler) convertMessagesFromSearch(ctx context.Context, slackMessages []slack.SearchMessage) []Message {
	resolver := ch.newUserResolver(ctx)
	var messages []Message
	warn := false

	for _, msg := range slackMessages {
		userName, realName, ok := resolver.resolve(msg.User)

		if !ok && msg.User == "" && msg.Username != "" {
			userName, realName, ok = getBotInfo(msg.Username)
		}

		if !ok {
			warn = true
		}

		threadTs, _ := extractThreadTS(msg.Permalink)

		timestamp, err := text.TimestampToIsoRFC3339(msg.Timestamp)
		if err != nil {
			ch.logger.Error("Failed to convert timestamp to RFC3339", zap.Error(err))
			continue
		}

		msgText := msg.Text
		if msgText == "" {
			msgText = text.BlocksToText(msg.Blocks)
		}
		msgText += text.AttachmentsTo2CSV(msgText, msg.Attachments)

		hasMedia := hasImageBlocks(msg.Blocks)

		messages = append(messages, Message{
			MsgID:     msg.Timestamp,
			UserID:    msg.User,
			UserName:  userName,
			RealName:  realName,
			Text:      text.ProcessText(msgText),
			Channel:   fmt.Sprintf("%s (#%s)", msg.Channel.ID, msg.Channel.Name),
			ThreadTs:  threadTs,
			Time:      timestamp,
			Permalink: msg.Permalink,
			Reactions: "",
			HasMedia:  hasMedia,
		})
	}

	if ready, err := ch.apiProvider.IsReady(); !ready {
		if warn && errors.Is(err, provider.ErrUsersNotReady) {
			ch.logger.Warn(
				"Slack users sync not ready; you may see raw UIDs instead of names and lose some functionality.",
				zap.Error(err),
			)
		}
	}
	return messages
}

func (ch *ConversationsHandler) parseParamsToolConversations(ctx context.Context, request mcp.CallToolRequest) (*conversationParams, error) {
	channel := strings.TrimSpace(request.GetString("channel_id", ""))

	var threadTs string
	if raw := strings.TrimSpace(request.GetString("thread_ts", "")); raw != "" {
		if ref, ok := parseSlackMessageURL(raw); ok {
			threadTs = ref.rootTS()
			if channel == "" {
				channel = ref.ChannelID
			}
		} else if ts, ok := normalizeSlackTS(raw); ok {
			threadTs = ts
		} else {
			return nil, fmt.Errorf("thread_ts must be a timestamp in format 1234567890.123456 or a full Slack message URL")
		}
	}

	if channel == "" {
		ch.logger.Error("channel_id missing in conversations params")
		return nil, errors.New("channel_id must be a string (or provide a full Slack message URL as thread_ts)")
	}

	limit := request.GetString("limit", "")
	limitProvided := false
	if args := request.GetArguments(); args != nil {
		_, limitProvided = args["limit"]
	}
	// Thread reads should not be truncated to today's messages by default.
	// The tool schema default is "1d" for channel history; when the caller did
	// not explicitly ask for a time window, fetch a numeric page instead.
	if threadTs != "" && !limitProvided {
		limit = ""
	}
	cursor := request.GetString("cursor", "")
	activity := request.GetBool("include_activity_messages", false)
	query := strings.TrimSpace(request.GetString("query", ""))
	includeThreadReplies := request.GetBool("include_thread_replies", false)

	var (
		paramLimit  int
		paramOldest string
		paramLatest string
		err         error
	)
	if isDurationLimit(limit) {
		paramLimit, paramOldest, paramLatest, err = limitByExpression(limit, defaultConversationsExpressionLimit)
		if err != nil {
			ch.logger.Error("Invalid duration limit", zap.String("limit", limit), zap.Error(err))
			return nil, err
		}
	} else if cursor == "" {
		numericDefault := defaultConversationsNumericLimit
		if threadTs != "" {
			numericDefault = defaultThreadNumericLimit
		}
		paramLimit, err = limitByNumeric(limit, numericDefault)
		if err != nil {
			ch.logger.Error("Invalid numeric limit", zap.String("limit", limit), zap.Error(err))
			return nil, err
		}
	}

	// Absolute timestamps win over relative duration limits.
	if raw := strings.TrimSpace(request.GetString("oldest_ts", "")); raw != "" {
		ts, ok := normalizeSlackTS(raw)
		if !ok {
			return nil, errors.New("oldest_ts must be in format 1234567890.123456")
		}
		paramOldest = ts
	}
	if raw := strings.TrimSpace(request.GetString("latest_ts", "")); raw != "" {
		ts, ok := normalizeSlackTS(raw)
		if !ok {
			return nil, errors.New("latest_ts must be in format 1234567890.123456")
		}
		paramLatest = ts
	}

	if strings.HasPrefix(channel, "#") || strings.HasPrefix(channel, "@") {
		if ready, err := ch.apiProvider.IsReady(); !ready {
			if errors.Is(err, provider.ErrUsersNotReady) {
				ch.logger.Warn(
					"WARNING: Slack users sync is not ready yet, you may experience some limited functionality and see UIDs instead of resolved names as well as unable to query users by their @handles. Users sync is part of channels sync and operations on channels depend on users collection (IM, MPIM). Please wait until users are synced and try again",
					zap.Error(err),
				)
			}
			if errors.Is(err, provider.ErrChannelsNotReady) {
				ch.logger.Warn(
					"WARNING: Slack channels sync is not ready yet, you may experience some limited functionality and be able to request conversation only by Channel ID, not by its name. Please wait until channels are synced and try again.",
					zap.Error(err),
				)
			}
			return nil, fmt.Errorf("channel %q not found in empty cache", channel)
		}
		// Use resolveChannelID which includes refresh-on-error logic
		resolvedChannel, err := ch.resolveChannelID(ctx, channel)
		if err != nil {
			return nil, err
		}
		channel = resolvedChannel
	}

	return &conversationParams{
		channel:              channel,
		threadTs:             threadTs,
		query:                query,
		limit:                paramLimit,
		oldest:               paramOldest,
		latest:               paramLatest,
		cursor:               cursor,
		activity:             activity,
		includeThreadReplies: includeThreadReplies,
	}, nil
}

// isDurationLimit reports whether limit is a human duration such as 30m, 2h,
// 3d or 1w rather than a plain message count.
func isDurationLimit(limit string) bool {
	l := strings.ToLower(strings.TrimSpace(limit))
	if l == "" {
		return false
	}
	return strings.HasSuffix(l, "min") || strings.HasSuffix(l, "d") ||
		strings.HasSuffix(l, "w") || strings.HasSuffix(l, "m") ||
		strings.HasSuffix(l, "h") || strings.HasSuffix(l, "s")
}

func (ch *ConversationsHandler) parseParamsToolAddMessage(ctx context.Context, request mcp.CallToolRequest) (*addMessageParams, error) {
	toolConfig := os.Getenv("SLACK_MCP_ADD_MESSAGE_TOOL")
	enabledTools := os.Getenv("SLACK_MCP_ENABLED_TOOLS")

	if toolConfig == "" {
		if !strings.Contains(enabledTools, "conversations_add_message") {
			ch.logger.Error("Add-message tool disabled by default")
			return nil, errors.New(
				"by default, the conversations_add_message tool is disabled to guard Slack workspaces against accidental spamming. " +
					"To enable it, set the SLACK_MCP_ADD_MESSAGE_TOOL environment variable to true, 1, or comma separated list of channels " +
					"to limit where the MCP can post messages, e.g. 'SLACK_MCP_ADD_MESSAGE_TOOL=C1234567890,D0987654321', 'SLACK_MCP_ADD_MESSAGE_TOOL=!C1234567890' " +
					"to enable all except one or 'SLACK_MCP_ADD_MESSAGE_TOOL=true' for all channels and DMs",
			)
		}
		toolConfig = "true"
	}

	channel := request.GetString("channel_id", "")
	if channel == "" {
		ch.logger.Error("channel_id missing in add-message params")
		return nil, errors.New("channel_id must be a string")
	}
	channel, err := ch.resolveChannelID(ctx, channel)
	if err != nil {
		ch.logger.Error("Channel not found", zap.String("channel", channel), zap.Error(err))
		return nil, err
	}
	if !isChannelAllowed(channel) {
		ch.logger.Warn("Add-message tool not allowed for channel", zap.String("channel", channel), zap.String("policy", toolConfig))
		return nil, fmt.Errorf("conversations_add_message tool is not allowed for channel %q, applied policy: %s", channel, toolConfig)
	}

	threadTs := request.GetString("thread_ts", "")
	if threadTs != "" && !strings.Contains(threadTs, ".") {
		ch.logger.Error("Invalid thread_ts format", zap.String("thread_ts", threadTs))
		return nil, errors.New("thread_ts must be a valid timestamp in format 1234567890.123456")
	}

	msgText := request.GetString("text", "")
	if msgText == "" {
		// Backward compatibility with "payload" parameter
		msgText = request.GetString("payload", "")
	}

	contentType := request.GetString("content_type", "text/markdown")
	if contentType != "text/plain" && contentType != "text/markdown" {
		ch.logger.Error("Invalid content_type", zap.String("content_type", contentType))
		return nil, errors.New("content_type must be either 'text/plain' or 'text/markdown'")
	}

	// Parse optional raw blocks JSON. Accepts blocks as either:
	// - A JSON string containing a blocks array: "blocks": "[{...}]"
	// - A raw JSON array (parsed by MCP SDK): "blocks": [{...}]
	var blocks []slack.Block
	args := request.GetArguments()
	if rawBlocks, ok := args["blocks"]; ok && rawBlocks != nil {
		var blocksJSON []byte
		switch v := rawBlocks.(type) {
		case string:
			if v != "" {
				blocksJSON = []byte(v)
			}
		default:
			// Raw JSON array/object passed directly - re-marshal to bytes
			var err error
			blocksJSON, err = json.Marshal(v)
			if err != nil {
				ch.logger.Error("Failed to marshal blocks argument", zap.Error(err))
				return nil, fmt.Errorf("blocks must be valid Slack Block Kit JSON: %w", err)
			}
		}
		if blocksJSON != nil {
			var slackBlocks slack.Blocks
			if err := json.Unmarshal(blocksJSON, &slackBlocks); err != nil {
				ch.logger.Error("Failed to parse blocks JSON", zap.Error(err))
				return nil, fmt.Errorf("blocks must be valid Slack Block Kit JSON: %w", err)
			}
			blocks = slackBlocks.BlockSet
		}
	}

	// Require either text or blocks
	if msgText == "" && blocks == nil {
		ch.logger.Error("Message text and blocks both missing")
		return nil, errors.New("either text or blocks must be provided")
	}

	return &addMessageParams{
		channel:     channel,
		threadTs:    threadTs,
		text:        msgText,
		contentType: contentType,
		blocks:      blocks,
	}, nil
}

func (ch *ConversationsHandler) parseParamsToolReaction(ctx context.Context, request mcp.CallToolRequest) (*addReactionParams, error) {
	toolConfig := os.Getenv("SLACK_MCP_REACTION_TOOL")
	enabledTools := os.Getenv("SLACK_MCP_ENABLED_TOOLS")

	if toolConfig == "" {
		if !strings.Contains(enabledTools, "reactions_add") && !strings.Contains(enabledTools, "reactions_remove") {
			ch.logger.Error("Reactions tool disabled by default")
			return nil, errors.New(
				"by default, the reactions tools are disabled to guard Slack workspaces against accidental spamming. " +
					"To enable them, set the SLACK_MCP_REACTION_TOOL environment variable to true, 1, or comma separated list of channels " +
					"to limit where the MCP can manage reactions, e.g. 'SLACK_MCP_REACTION_TOOL=C1234567890,D0987654321', 'SLACK_MCP_REACTION_TOOL=!C1234567890' " +
					"to enable all except one or 'SLACK_MCP_REACTION_TOOL=true' for all channels and DMs",
			)
		}
		toolConfig = "true"
	}

	channel := request.GetString("channel_id", "")
	if channel == "" {
		return nil, errors.New("channel_id is required")
	}
	channel, err := ch.resolveChannelID(ctx, channel)
	if err != nil {
		ch.logger.Error("Channel not found", zap.String("channel", channel), zap.Error(err))
		return nil, err
	}
	if !isChannelAllowedForConfig(channel, toolConfig) {
		ch.logger.Warn("Reactions tool not allowed for channel", zap.String("channel", channel), zap.String("policy", toolConfig))
		return nil, fmt.Errorf("reactions tools are not allowed for channel %q, applied policy: %s", channel, toolConfig)
	}

	timestamp := request.GetString("timestamp", "")
	if timestamp == "" {
		return nil, errors.New("timestamp is required")
	}

	emoji := strings.Trim(request.GetString("emoji", ""), ":")
	if emoji == "" {
		return nil, errors.New("emoji is required")
	}

	return &addReactionParams{
		channel:   channel,
		timestamp: timestamp,
		emoji:     emoji,
	}, nil
}

func (ch *ConversationsHandler) parseParamsToolFilesGet(request mcp.CallToolRequest) (*filesGetParams, error) {
	toolConfig := os.Getenv("SLACK_MCP_ATTACHMENT_TOOL")
	enabledTools := os.Getenv("SLACK_MCP_ENABLED_TOOLS")

	if toolConfig == "" {
		if !strings.Contains(enabledTools, "attachment_get_data") {
			ch.logger.Error("Attachment tool disabled by default")
			return nil, errors.New(
				"by default, the attachment_get_data tool is disabled. " +
					"To enable it, set the SLACK_MCP_ATTACHMENT_TOOL environment variable to true or 1",
			)
		}
		toolConfig = "true"
	}
	if toolConfig != "true" && toolConfig != "1" && toolConfig != "yes" {
		ch.logger.Error("Attachment tool disabled", zap.String("config", toolConfig))
		return nil, errors.New("SLACK_MCP_ATTACHMENT_TOOL must be set to 'true', '1', or 'yes' to enable")
	}

	fileID := request.GetString("file_id", "")
	if fileID == "" {
		return nil, errors.New("file_id is required")
	}

	return &filesGetParams{
		fileID: fileID,
	}, nil
}

func (ch *ConversationsHandler) parseParamsToolUsersSearch(request mcp.CallToolRequest) (*usersSearchParams, error) {
	query := strings.TrimSpace(request.GetString("query", ""))
	if query == "" {
		return nil, errors.New("query is required")
	}

	limit := request.GetInt("limit", 10)
	if limit <= 0 {
		limit = 10
	}
	if limit > 100 {
		limit = 100
	}

	return &usersSearchParams{
		query: query,
		limit: limit,
	}, nil
}

func (ch *ConversationsHandler) parseParamsToolReactionsList(ctx context.Context, request mcp.CallToolRequest) (*reactionsListParams, error) {
	channel := strings.TrimSpace(request.GetString("channel_id", ""))
	if channel == "" {
		return nil, errors.New("channel_id must be a string")
	}
	id, err := ch.resolveChannelID(ctx, channel)
	if err != nil {
		return nil, err
	}
	timestamp := strings.TrimSpace(request.GetString("timestamp", ""))
	ts, ok := normalizeSlackTS(timestamp)
	if !ok {
		return nil, errors.New("timestamp must be in format 1234567890.123456")
	}
	return &reactionsListParams{channel: id, timestamp: ts}, nil
}

func (ch *ConversationsHandler) parseParamsToolPinsList(ctx context.Context, request mcp.CallToolRequest) (*pinsListParams, error) {
	channel := strings.TrimSpace(request.GetString("channel_id", ""))
	if channel == "" {
		return nil, errors.New("channel_id must be a string")
	}
	id, err := ch.resolveChannelID(ctx, channel)
	if err != nil {
		return nil, err
	}
	return &pinsListParams{channel: id}, nil
}

func (ch *ConversationsHandler) parseParamsToolMembers(ctx context.Context, request mcp.CallToolRequest) (*membersParams, error) {
	channel := strings.TrimSpace(request.GetString("channel_id", ""))
	if channel == "" {
		return nil, errors.New("channel_id must be a string")
	}
	id, err := ch.resolveChannelID(ctx, channel)
	if err != nil {
		return nil, err
	}
	limit := request.GetInt("limit", 100)
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	return &membersParams{
		channel: id,
		limit:   limit,
		cursor:  request.GetString("cursor", ""),
	}, nil
}

func (ch *ConversationsHandler) parseParamsToolEditMessage(ctx context.Context, request mcp.CallToolRequest) (*editMessageParams, error) {
	config, err := requiredWriteToolConfig("SLACK_MCP_EDIT_MESSAGE_TOOL", "conversations_edit_message")
	if err != nil {
		return nil, err
	}
	channel := strings.TrimSpace(request.GetString("channel_id", ""))
	if channel == "" {
		return nil, errors.New("channel_id must be a string")
	}
	id, err := ch.resolveChannelID(ctx, channel)
	if err != nil {
		return nil, err
	}
	if !isChannelAllowedForConfig(id, config) {
		return nil, fmt.Errorf("conversations_edit_message is not allowed for channel %q, applied policy: %s", id, config)
	}
	timestamp := strings.TrimSpace(request.GetString("timestamp", ""))
	ts, ok := normalizeSlackTS(timestamp)
	if !ok {
		return nil, errors.New("timestamp must be in format 1234567890.123456")
	}
	msgText := request.GetString("text", "")
	if msgText == "" {
		msgText = request.GetString("payload", "")
	}
	contentType := request.GetString("content_type", "text/markdown")
	if contentType != "text/plain" && contentType != "text/markdown" {
		return nil, errors.New("content_type must be either 'text/plain' or 'text/markdown'")
	}
	blocks, err := parseBlocksArg(request)
	if err != nil {
		return nil, err
	}
	if msgText == "" && blocks == nil {
		return nil, errors.New("either text or blocks must be provided")
	}
	return &editMessageParams{
		channel:     id,
		timestamp:   ts,
		text:        msgText,
		contentType: contentType,
		blocks:      blocks,
	}, nil
}

func (ch *ConversationsHandler) parseParamsToolDeleteMessage(ctx context.Context, request mcp.CallToolRequest) (*deleteMessageParams, error) {
	config, err := requiredWriteToolConfig("SLACK_MCP_DELETE_MESSAGE_TOOL", "conversations_delete_message")
	if err != nil {
		return nil, err
	}
	channel := strings.TrimSpace(request.GetString("channel_id", ""))
	if channel == "" {
		return nil, errors.New("channel_id must be a string")
	}
	id, err := ch.resolveChannelID(ctx, channel)
	if err != nil {
		return nil, err
	}
	if !isChannelAllowedForConfig(id, config) {
		return nil, fmt.Errorf("conversations_delete_message is not allowed for channel %q, applied policy: %s", id, config)
	}
	timestamp := strings.TrimSpace(request.GetString("timestamp", ""))
	ts, ok := normalizeSlackTS(timestamp)
	if !ok {
		return nil, errors.New("timestamp must be in format 1234567890.123456")
	}
	return &deleteMessageParams{channel: id, timestamp: ts}, nil
}

func (ch *ConversationsHandler) parseParamsToolUploadFile(ctx context.Context, request mcp.CallToolRequest) (*uploadFileParams, error) {
	config, err := requiredWriteToolConfig("SLACK_MCP_UPLOAD_FILE_TOOL", "files_upload")
	if err != nil {
		return nil, err
	}
	channel := strings.TrimSpace(request.GetString("channel_id", ""))
	if channel == "" {
		return nil, errors.New("channel_id must be a string")
	}
	id, err := ch.resolveChannelID(ctx, channel)
	if err != nil {
		return nil, err
	}
	if !isChannelAllowedForConfig(id, config) {
		return nil, fmt.Errorf("files_upload is not allowed for channel %q, applied policy: %s", id, config)
	}
	filename := strings.TrimSpace(request.GetString("filename", ""))
	if filename == "" {
		return nil, errors.New("filename is required")
	}
	content := request.GetString("content", "")
	content64 := strings.TrimSpace(request.GetString("content_base64", ""))
	if content == "" && content64 == "" {
		return nil, errors.New("provide either content or content_base64")
	}
	var threadTs string
	if raw := strings.TrimSpace(request.GetString("thread_ts", "")); raw != "" {
		ts, ok := normalizeSlackTS(raw)
		if !ok {
			return nil, errors.New("thread_ts must be in format 1234567890.123456")
		}
		threadTs = ts
	}
	return &uploadFileParams{
		channel:   id,
		filename:  filename,
		content:   content,
		content64: content64,
		title:     request.GetString("title", ""),
		comment:   request.GetString("initial_comment", ""),
		threadTs:  threadTs,
	}, nil
}

func (ch *ConversationsHandler) parseParamsToolFilesList(ctx context.Context, request mcp.CallToolRequest) (*filesListParams, error) {
	channel := strings.TrimSpace(request.GetString("channel_id", ""))
	if channel != "" {
		id, err := ch.resolveChannelID(ctx, channel)
		if err != nil {
			return nil, err
		}
		channel = id
	}
	types := strings.TrimSpace(request.GetString("types", "all"))
	if types == "" {
		types = "all"
	}
	limit := request.GetInt("limit", 100)
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	page := request.GetInt("page", 1)
	if page < 1 {
		page = 1
	}
	return &filesListParams{channel: channel, types: types, limit: limit, page: page}, nil
}

func (ch *ConversationsHandler) parseParamsToolCanvasRead(request mcp.CallToolRequest) (*canvasReadParams, error) {
	fileID := extractSlackFileID(request.GetString("file_id", ""))
	if fileID == "" {
		return nil, errors.New("file_id is required")
	}
	return &canvasReadParams{fileID: fileID}, nil
}

func (ch *ConversationsHandler) parseParamsToolListsItems(request mcp.CallToolRequest) (*listsItemsParams, error) {
	listID := extractSlackFileID(request.GetString("list_id", ""))
	if listID == "" {
		return nil, errors.New("list_id is required (the F... id from the List URL)")
	}
	limit := request.GetInt("limit", 100)
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	return &listsItemsParams{
		listID: listID,
		limit:  limit,
		cursor: request.GetString("cursor", ""),
	}, nil
}

func requiredWriteToolConfig(envVar, toolName string) (string, error) {
	config := strings.TrimSpace(os.Getenv(envVar))
	if config == "" {
		return "", fmt.Errorf("%s is disabled; set %s to true, 1, or a comma-separated channel list", toolName, envVar)
	}
	return config, nil
}

func parseBlocksArg(request mcp.CallToolRequest) ([]slack.Block, error) {
	args := request.GetArguments()
	rawBlocks, ok := args["blocks"]
	if !ok || rawBlocks == nil {
		return nil, nil
	}
	var blocksJSON []byte
	switch v := rawBlocks.(type) {
	case string:
		if v == "" {
			return nil, nil
		}
		blocksJSON = []byte(v)
	default:
		var err error
		blocksJSON, err = json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("blocks must be valid Slack Block Kit JSON: %w", err)
		}
	}
	var slackBlocks slack.Blocks
	if err := json.Unmarshal(blocksJSON, &slackBlocks); err != nil {
		return nil, fmt.Errorf("blocks must be valid Slack Block Kit JSON: %w", err)
	}
	return slackBlocks.BlockSet, nil
}

func extractSlackFileID(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	re := regexp.MustCompile(`(?i)\bF[A-Z0-9]{5,}\b`)
	if id := re.FindString(raw); id != "" {
		return strings.ToUpper(id)
	}
	return raw
}

func buildMessageOptions(msgText, contentType string, blocks []slack.Block) ([]slack.MsgOption, error) {
	if blocks != nil {
		options := []slack.MsgOption{slack.MsgOptionBlocks(blocks...)}
		if msgText != "" {
			options = append(options, slack.MsgOptionText(msgText, false))
		}
		return options, nil
	}
	if msgText == "" {
		return nil, errors.New("either text or blocks must be provided")
	}
	switch contentType {
	case "text/plain":
		return []slack.MsgOption{slack.MsgOptionDisableMarkdown(), slack.MsgOptionText(msgText, false)}, nil
	case "text/markdown":
		blocks, err := slackGoUtil.ConvertMarkdownTextToBlocks(msgText)
		if err != nil {
			return []slack.MsgOption{slack.MsgOptionDisableMarkdown(), slack.MsgOptionText(msgText, false)}, nil
		}
		return []slack.MsgOption{slack.MsgOptionBlocks(blocks...)}, nil
	default:
		return nil, errors.New("content_type must be either 'text/plain' or 'text/markdown'")
	}
}

func (ch *ConversationsHandler) parseParamsToolUnreads(request mcp.CallToolRequest) *unreadsParams {
	return &unreadsParams{
		includeMessages:       request.GetBool("include_messages", true),
		channelTypes:          request.GetString("channel_types", "all"),
		maxChannels:           request.GetInt("max_channels", 50),
		maxMessagesPerChannel: request.GetInt("max_messages_per_channel", 10),
		mentionsOnly:          request.GetBool("mentions_only", false),
		includeMuted:          request.GetBool("include_muted", false),
	}
}

func (ch *ConversationsHandler) parseParamsToolMark(request mcp.CallToolRequest) (*markParams, error) {
	toolConfig := os.Getenv("SLACK_MCP_MARK_TOOL")
	if toolConfig == "" {
		ch.logger.Error("Mark tool disabled by default")
		return nil, errors.New(
			"by default, the conversations_mark tool is disabled to prevent accidental marking of messages as read. " +
				"To enable it, set the SLACK_MCP_MARK_TOOL environment variable to true or 1, " +
				"e.g. 'SLACK_MCP_MARK_TOOL=true'",
		)
	}
	if toolConfig != "1" && toolConfig != "true" && toolConfig != "yes" {
		ch.logger.Error("Mark tool disabled by config", zap.String("config", toolConfig))
		return nil, errors.New(
			"the conversations_mark tool is disabled. " +
				"To enable it, set the SLACK_MCP_MARK_TOOL environment variable to true or 1",
		)
	}

	channel := request.GetString("channel_id", "")
	if channel == "" {
		ch.logger.Error("channel_id missing in mark params")
		return nil, errors.New("channel_id is required")
	}

	// Resolve channel name to ID if needed
	if strings.HasPrefix(channel, "#") || strings.HasPrefix(channel, "@") {
		channelsMaps := ch.apiProvider.ProvideChannelsMaps()
		chn, ok := channelsMaps.ChannelsInv[channel]
		if !ok {
			ch.logger.Error("Channel not found", zap.String("channel", channel))
			return nil, fmt.Errorf("channel %q not found", channel)
		}
		channel = channelsMaps.Channels[chn].ID
	}

	ts := request.GetString("ts", "")

	return &markParams{
		channel: channel,
		ts:      ts,
	}, nil
}
func (ch *ConversationsHandler) parseParamsToolSearch(ctx context.Context, req mcp.CallToolRequest) (*searchParams, error) {
	rawQuery := strings.TrimSpace(req.GetString("search_query", ""))
	params := &searchParams{
		query: rawQuery,
		limit: req.GetInt("limit", 100),
		page:  1,
	}
	limitProvided := false
	if args := req.GetArguments(); args != nil {
		_, limitProvided = args["limit"]
	}

	// A full Slack message URL is resolved directly rather than sent to
	// Slack's search API. Other filters are ignored for URL searches, as
	// documented on the tool.
	if ref, ok := parseSlackMessageURL(rawQuery); ok {
		params.messageRef = ref
		if !limitProvided {
			params.limit = defaultThreadNumericLimit
		}
	} else {
		freeText, filters := splitQuery(rawQuery)
		params.freeText = freeText
		params.filters = filters

		if req.GetBool("filter_threads_only", false) {
			addFilter(filters, "is", "thread")
		}

		rawThreadTs := strings.TrimSpace(req.GetString("thread_ts", ""))
		if rawThreadTs != "" {
			if ref, ok := parseSlackMessageURL(rawThreadTs); ok {
				params.threadTs = ref.rootTS()
				if params.threadChannel == "" {
					params.threadChannel = ref.ChannelID
				}
			} else if ts, ok := normalizeSlackTS(rawThreadTs); ok {
				params.threadTs = ts
			} else {
				return nil, fmt.Errorf("thread_ts must be a timestamp in format 1234567890.123456 or a full Slack message URL")
			}
		}

		if chName := strings.TrimSpace(req.GetString("filter_in_channel", "")); chName != "" {
			if isSlackChannelID(chName) {
				// Channel IDs are enough for a thread search and do not need
				// the channels cache. For a normal search we still need the
				// name for Slack's in: filter.
				params.threadChannel = chName
				if params.threadTs == "" {
					f, err := ch.paramFormatChannel(chName)
					if err != nil {
						ch.logger.Error("Invalid channel filter", zap.String("filter", chName), zap.Error(err))
						return nil, err
					}
					addFilter(filters, "in", f)
				}
			} else {
				f, err := ch.paramFormatChannel(chName)
				if err != nil {
					ch.logger.Error("Invalid channel filter", zap.String("filter", chName), zap.Error(err))
					return nil, err
				}
				id, err := ch.resolveChannelID(ctx, chName)
				if err != nil {
					ch.logger.Error("Invalid channel filter", zap.String("filter", chName), zap.Error(err))
					return nil, err
				}
				params.threadChannel = id
				if params.threadTs == "" {
					addFilter(filters, "in", f)
				}
			}
		} else if im := req.GetString("filter_in_im_or_mpim", ""); im != "" {
			f, err := ch.paramFormatUser(ctx, im)
			if err != nil {
				ch.logger.Error("Invalid IM/MPIM filter", zap.String("filter", im), zap.Error(err))
				return nil, err
			}
			addFilter(filters, "in", f)
		}
		if with := req.GetString("filter_users_with", ""); with != "" {
			f, err := ch.paramFormatUser(ctx, with)
			if err != nil {
				ch.logger.Error("Invalid with-user filter", zap.String("filter", with), zap.Error(err))
				return nil, err
			}
			addFilter(filters, "with", f)
		}
		if from := req.GetString("filter_users_from", ""); from != "" {
			f, err := ch.paramFormatUser(ctx, from)
			if err != nil {
				ch.logger.Error("Invalid from-user filter", zap.String("filter", from), zap.Error(err))
				return nil, err
			}
			addFilter(filters, "from", f)
		}

		dateMap, err := buildDateFilters(
			req.GetString("filter_date_before", ""),
			req.GetString("filter_date_after", ""),
			req.GetString("filter_date_on", ""),
			req.GetString("filter_date_during", ""),
		)
		if err != nil {
			ch.logger.Error("Invalid date filters", zap.Error(err))
			return nil, err
		}
		for key, val := range dateMap {
			addFilter(filters, key, val)
		}
		if tz := strings.TrimSpace(req.GetString("filter_date_timezone", "")); tz != "" {
			loc, err := time.LoadLocation(tz)
			if err != nil {
				return nil, fmt.Errorf("invalid filter_date_timezone %q: %v", tz, err)
			}
			params.dateLocation = loc
		}
		params.query = buildQuery(freeText, filters)
	}

	cursor := req.GetString("cursor", "")
	if cursor != "" {
		decoded, err := base64.StdEncoding.DecodeString(cursor)
		if err != nil {
			ch.logger.Error("Invalid cursor decoding", zap.String("cursor", cursor), zap.Error(err))
			return nil, fmt.Errorf("invalid cursor: %v", err)
		}
		parts := strings.SplitN(string(decoded), ":", 2)
		if len(parts) != 2 {
			ch.logger.Error("Invalid cursor format", zap.String("cursor", cursor))
			return nil, fmt.Errorf("invalid cursor: %v", cursor)
		}
		params.cursorMode = parts[0]
		params.cursor = parts[1]
		switch parts[0] {
		case "page":
			page, err := strconv.Atoi(parts[1])
			if err != nil || page < 1 {
				ch.logger.Error("Invalid cursor page", zap.String("cursor", cursor), zap.Error(err))
				return nil, fmt.Errorf("invalid cursor page: %v", err)
			}
			params.page = page
		case "thread", "tsearch":
			// Handled by the thread-based search paths.
		default:
			ch.logger.Error("Invalid cursor mode", zap.String("cursor", cursor))
			return nil, fmt.Errorf("invalid cursor: %v", cursor)
		}
	}

	if params.messageRef == nil && params.threadTs == "" && params.cursorMode != "" && params.cursorMode != "page" {
		return nil, fmt.Errorf("cursor was produced by a thread search; pass the same thread_ts or Slack URL")
	}
	if params.threadTs != "" && params.threadChannel == "" {
		return nil, errors.New("filter_in_channel is required when thread_ts is set (or use a full Slack message URL)")
	}

	ch.logger.Debug("Search parameters built",
		zap.String("query", params.query),
		zap.String("thread_ts", params.threadTs),
		zap.Int("limit", params.limit),
		zap.Int("page", params.page),
	)
	return params, nil
}

// Slack user IDs may begin with U or W: https://docs.slack.dev/changelog/2016/08/11/user-id-format-changes
func isSlackUserIDPrefix(s string) bool {
	return strings.HasPrefix(s, "U") || strings.HasPrefix(s, "W")
}

func (ch *ConversationsHandler) paramFormatUser(ctx context.Context, raw string) (string, error) {
	users := ch.apiProvider.ProvideUsersMap()
	raw = strings.TrimSpace(raw)
	if isSlackUserIDPrefix(raw) {
		u, ok := users.Users[raw]
		if !ok {
			// Targeted fetch: single users.info call instead of full cache rebuild
			patched, err := ch.apiProvider.PatchUser(ctx, raw)
			if err != nil {
				ch.logger.Debug("Targeted user fetch failed, user not found",
					zap.String("user_id", raw), zap.Error(err))
				return "", fmt.Errorf("user %q not found", raw)
			}
			return fmt.Sprintf("<@%s>", patched.ID), nil
		}
		return fmt.Sprintf("<@%s>", u.ID), nil
	}
	if strings.HasPrefix(raw, "<@") {
		raw = raw[2:]
	}
	if strings.HasPrefix(raw, "@") {
		raw = raw[1:]
	}
	uid, ok := users.UsersInv[raw]
	if !ok {
		return "", fmt.Errorf("user %q not found", raw)
	}
	return fmt.Sprintf("<@%s>", uid), nil
}

func (ch *ConversationsHandler) paramFormatChannel(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	cms := ch.apiProvider.ProvideChannelsMaps()
	if strings.HasPrefix(raw, "#") {
		if id, ok := cms.ChannelsInv[raw]; ok {
			return cms.Channels[id].Name, nil
		}
		return "", fmt.Errorf("channel %q not found", raw)
	}
	// Handle both C (standard channels) and G (private groups/channels) prefixes
	if strings.HasPrefix(raw, "C") || strings.HasPrefix(raw, "G") {
		if chn, ok := cms.Channels[raw]; ok {
			return chn.Name, nil
		}
		return "", fmt.Errorf("channel %q not found", raw)
	}
	return "", fmt.Errorf("invalid channel format: %q", raw)
}

func marshalMessagesToCSV(messages []Message) (*mcp.CallToolResult, error) {
	csvBytes, err := gocsv.MarshalBytes(&messages)
	if err != nil {
		return nil, err
	}
	return mcp.NewToolResultText(string(csvBytes)), nil
}

func marshalRowsToCSV[T any](rows []T) (*mcp.CallToolResult, error) {
	csvBytes, err := gocsv.MarshalBytes(&rows)
	if err != nil {
		return nil, err
	}
	return mcp.NewToolResultText(string(csvBytes)), nil
}

func getUserInfo(userID string, usersMap map[string]slack.User) (userName, realName string, ok bool) {
	if u, ok := usersMap[userID]; ok {
		return u.Name, u.RealName, true
	}
	return userID, userID, false
}

// userResolver resolves user IDs to names, fetching unknown users from the
// Slack API on demand. It caches the snapshot locally and remembers which IDs
// it already tried to fetch, so a user that doesn't exist in Slack is only
// looked up once per batch rather than once per message.
type userResolver struct {
	apiProvider  *provider.ApiProvider
	ctx          context.Context
	usersMap     *provider.UsersCache
	attemptedIDs map[string]bool
}

func (ch *ConversationsHandler) newUserResolver(ctx context.Context) *userResolver {
	return &userResolver{
		apiProvider:  ch.apiProvider,
		ctx:          ctx,
		usersMap:     ch.apiProvider.ProvideUsersMap(),
		attemptedIDs: make(map[string]bool),
	}
}

func (r *userResolver) resolve(userID string) (userName, realName string, ok bool) {
	if u, ok := r.usersMap.Users[userID]; ok {
		return u.Name, u.RealName, true
	}
	if userID == "" || r.attemptedIDs[userID] {
		return userID, userID, false
	}
	r.attemptedIDs[userID] = true
	patched, err := r.apiProvider.PatchUser(r.ctx, userID)
	if err != nil {
		return userID, userID, false
	}
	r.usersMap = r.apiProvider.ProvideUsersMap()
	return patched.Name, patched.RealName, true
}

func getBotInfo(botID string) (userName, realName string, ok bool) {
	return botID, botID, true
}

// workspaceBaseURL returns the team URL used to build message permalinks.
// AuthTest is cached by the Slack client, so this does not add a request per
// message.
func (ch *ConversationsHandler) workspaceBaseURL() string {
	ar, err := ch.apiProvider.Slack().AuthTest()
	if err != nil || ar == nil || ar.URL == "" {
		return ""
	}
	return strings.TrimSuffix(ar.URL, "/")
}

func slackPermalink(baseURL, channel, ts, threadTs string) string {
	if baseURL == "" || channel == "" || ts == "" {
		return ""
	}
	link := fmt.Sprintf("%s/archives/%s/p%s", baseURL, channel, strings.ReplaceAll(ts, ".", ""))
	if threadTs != "" && threadTs != ts {
		link += "?thread_ts=" + url.QueryEscape(threadTs) + "&cid=" + url.QueryEscape(channel)
	}
	return link
}

// formatReactionUsers returns "reaction:user1,user2|..." so callers can see
// who reacted, not just the counts.
func formatReactionUsers(reactions []slack.ItemReaction, resolver *userResolver) string {
	var parts []string
	for _, r := range reactions {
		if len(r.Users) == 0 {
			continue
		}
		names := make([]string, 0, len(r.Users))
		for _, uid := range r.Users {
			name, _, _ := resolver.resolve(uid)
			if name == "" {
				name = uid
			}
			names = append(names, name)
		}
		parts = append(parts, fmt.Sprintf("%s:%s", r.Name, strings.Join(names, ",")))
	}
	return strings.Join(parts, "|")
}

func limitByNumeric(limit string, defaultLimit int) (int, error) {
	if limit == "" {
		return defaultLimit, nil
	}
	n, err := strconv.Atoi(limit)
	if err != nil {
		return 0, fmt.Errorf("invalid numeric limit: %q", limit)
	}
	return n, nil
}

func limitByExpression(limit, defaultLimit string) (slackLimit int, oldest, latest string, err error) {
	if limit == "" {
		limit = defaultLimit
	}
	limit = strings.ToLower(strings.TrimSpace(limit))
	if len(limit) < 2 {
		return 0, "", "", fmt.Errorf("invalid duration limit %q: too short", limit)
	}

	var (
		suffix string
		numStr string
	)
	if strings.HasSuffix(limit, "min") {
		suffix = "min"
		numStr = strings.TrimSuffix(limit, "min")
	} else {
		suffix = limit[len(limit)-1:]
		numStr = limit[:len(limit)-1]
	}
	n, err := strconv.Atoi(numStr)
	if err != nil || n <= 0 {
		return 0, "", "", fmt.Errorf("invalid duration limit %q: must be a positive integer followed by 's', 'min', 'h', 'd', 'w', or 'm'", limit)
	}

	now := time.Now()
	loc := now.Location()
	startOfToday := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)

	var oldestTime time.Time
	switch suffix {
	case "s":
		oldestTime = now.Add(-time.Duration(n) * time.Second)
	case "min":
		oldestTime = now.Add(-time.Duration(n) * time.Minute)
	case "h":
		oldestTime = now.Add(-time.Duration(n) * time.Hour)
	case "d":
		oldestTime = startOfToday.AddDate(0, 0, -n+1)
	case "w":
		oldestTime = startOfToday.AddDate(0, 0, -n*7+1)
	case "m":
		oldestTime = startOfToday.AddDate(0, -n, 0)
	default:
		return 0, "", "", fmt.Errorf("invalid duration limit %q: must end in 's', 'min', 'h', 'd', 'w', or 'm'", limit)
	}
	latest = fmt.Sprintf("%d.000000", now.Unix())
	oldest = fmt.Sprintf("%d.000000", oldestTime.Unix())
	return 100, oldest, latest, nil
}

func extractThreadTS(rawurl string) (string, error) {
	u, err := url.Parse(rawurl)
	if err != nil {
		return "", err
	}
	return u.Query().Get("thread_ts"), nil
}

// normalizeSlackTS accepts either a regular Slack timestamp
// ("1234567890.123456"), the p-prefixed form used in permalinks
// ("p1234567890123456"), or the bare 16-digit form and returns the regular
// dotted timestamp.
func normalizeSlackTS(raw string) (string, bool) {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "p")
	if s == "" {
		return "", false
	}
	if strings.Contains(s, ".") {
		parts := strings.SplitN(s, ".", 2)
		if len(parts) != 2 || !allDigits(parts[0]) || !allDigits(parts[1]) {
			return "", false
		}
		return s, true
	}
	if !allDigits(s) || len(s) <= 6 {
		return "", false
	}
	return s[:len(s)-6] + "." + s[len(s)-6:], true
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func isSlackChannelID(s string) bool {
	return strings.HasPrefix(s, "C") || strings.HasPrefix(s, "G") || strings.HasPrefix(s, "D")
}

// parseSlackMessageURL parses the permalink formats Slack uses in its web and
// desktop clients:
//
//	https://workspace.slack.com/archives/C123/p1234567890123456
//	https://workspace.slack.com/archives/C123/p1234567890123456?thread_ts=1234567890.123456
//	https://app.slack.com/client/T123/C123/thread/C123-1234567890.123456
func parseSlackMessageURL(raw string) (*slackMessageRef, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil, false
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, false
	}
	host := strings.ToLower(u.Hostname())
	if host != "slack.com" && !strings.HasSuffix(host, ".slack.com") {
		return nil, false
	}

	ref := &slackMessageRef{}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	for i, part := range parts {
		switch {
		case part == "archives" && i+2 < len(parts):
			ref.ChannelID = parts[i+1]
			if strings.HasPrefix(parts[i+2], "p") {
				if ts, ok := normalizeSlackTS(parts[i+2]); ok {
					ref.Timestamp = ts
				}
			}
		case part == "thread" && i+1 < len(parts):
			// app.slack.com/client/T123/C123/thread/C123-1234567890.123456
			seg := parts[i+1]
			if idx := strings.LastIndex(seg, "-"); idx > 0 {
				ref.ChannelID = seg[:idx]
				if ts, ok := normalizeSlackTS(seg[idx+1:]); ok {
					ref.Timestamp = ts
				}
			}
		}
	}

	q := u.Query()
	if ref.ChannelID == "" {
		ref.ChannelID = q.Get("cid")
	}
	if t := q.Get("thread_ts"); t != "" {
		if ts, ok := normalizeSlackTS(t); ok {
			ref.ThreadTS = ts
		}
	}

	if ref.ChannelID == "" || !isSlackChannelID(ref.ChannelID) || ref.Timestamp == "" {
		return nil, false
	}
	return ref, true
}

// filterMessagesByQuery keeps messages whose text or author matches every
// whitespace-separated term in query.
func filterMessagesByQuery(messages []Message, query string) []Message {
	terms := strings.Fields(strings.ToLower(query))
	if len(terms) == 0 {
		return messages
	}
	out := make([]Message, 0, len(messages))
	for _, msg := range messages {
		haystack := strings.ToLower(msg.Text + " " + msg.UserName + " " + msg.RealName)
		matched := true
		for _, term := range terms {
			if !strings.Contains(haystack, term) {
				matched = false
				break
			}
		}
		if matched {
			out = append(out, msg)
		}
	}
	return out
}

// threadMessageMatches applies the parsed search query to a thread message.
// Slack's search API cannot target a single thread, so these filters mirror the
// search syntax locally.
func threadMessageMatches(msg slack.Message, terms []string, filters map[string][]string, loc *time.Location) bool {
	body := msg.Text
	if body == "" {
		body = text.BlocksToText(msg.Blocks)
	}
	haystack := strings.ToLower(body)
	for _, term := range terms {
		if !strings.Contains(haystack, strings.ToLower(term)) {
			return false
		}
	}

	for _, value := range filters["from"] {
		uid := strings.TrimSuffix(strings.TrimPrefix(value, "<@"), ">")
		if msg.User != uid {
			return false
		}
	}

	return slackMessageMatchesDateFilters(msg.Timestamp, filters, loc)
}

func slackMessageMatchesDateFilters(ts string, filters map[string][]string, loc *time.Location) bool {
	msgTime, ok := parseSlackTimestamp(ts)
	if !ok {
		return true
	}
	if loc != nil {
		msgTime = msgTime.In(loc)
	} else {
		msgTime = msgTime.UTC()
	}
	msgDate := time.Date(msgTime.Year(), msgTime.Month(), msgTime.Day(), 0, 0, 0, 0, time.UTC)

	for _, value := range filters["on"] {
		d, err := time.Parse("2006-01-02", value)
		if err == nil && !msgDate.Equal(d) {
			return false
		}
	}
	for _, value := range filters["during"] {
		d, err := time.Parse("2006-01-02", value)
		if err == nil && !msgDate.Equal(d) {
			return false
		}
	}
	for _, value := range filters["after"] {
		d, err := time.Parse("2006-01-02", value)
		if err == nil && msgDate.Before(d) {
			return false
		}
	}
	for _, value := range filters["before"] {
		d, err := time.Parse("2006-01-02", value)
		if err == nil && !msgDate.Before(d) {
			return false
		}
	}
	return true
}

func parseSlackTimestamp(ts string) (time.Time, bool) {
	sec, err := strconv.ParseFloat(ts, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(int64(sec), 0).UTC(), true
}

func parseFlexibleDate(dateStr string) (time.Time, string, error) {
	dateStr = strings.TrimSpace(dateStr)
	standardFormats := []string{
		"2006-01-02",      // YYYY-MM-DD
		"2006/01/02",      // YYYY/MM/DD
		"01-02-2006",      // MM-DD-YYYY
		"01/02/2006",      // MM/DD/YYYY
		"02-01-2006",      // DD-MM-YYYY
		"02/01/2006",      // DD/MM/YYYY
		"Jan 2, 2006",     // Jan 2, 2006
		"January 2, 2006", // January 2, 2006
		"2 Jan 2006",      // 2 Jan 2006
		"2 January 2006",  // 2 January 2006
	}
	for _, fmtStr := range standardFormats {
		if t, err := time.Parse(fmtStr, dateStr); err == nil {
			return t, t.Format("2006-01-02"), nil
		}
	}

	monthMap := map[string]int{
		"january": 1, "jan": 1,
		"february": 2, "feb": 2,
		"march": 3, "mar": 3,
		"april": 4, "apr": 4,
		"may":  5,
		"june": 6, "jun": 6,
		"july": 7, "jul": 7,
		"august": 8, "aug": 8,
		"september": 9, "sep": 9, "sept": 9,
		"october": 10, "oct": 10,
		"november": 11, "nov": 11,
		"december": 12, "dec": 12,
	}

	// Month-Year patterns
	monthYear := regexp.MustCompile(`^(\d{4})\s+([A-Za-z]+)$|^([A-Za-z]+)\s+(\d{4})$`)
	if m := monthYear.FindStringSubmatch(dateStr); m != nil {
		var year int
		var monStr string
		if m[1] != "" && m[2] != "" {
			year, _ = strconv.Atoi(m[1])
			monStr = strings.ToLower(m[2])
		} else {
			year, _ = strconv.Atoi(m[4])
			monStr = strings.ToLower(m[3])
		}
		if mon, ok := monthMap[monStr]; ok {
			t := time.Date(year, time.Month(mon), 1, 0, 0, 0, 0, time.UTC)
			return t, t.Format("2006-01-02"), nil
		}
	}

	// Day-Month-Year and Month-Day-Year patterns
	dmy1 := regexp.MustCompile(`^(\d{1,2})[-\s]+([A-Za-z]+)[-\s]+(\d{4})$`)
	if m := dmy1.FindStringSubmatch(dateStr); m != nil {
		day, _ := strconv.Atoi(m[1])
		year, _ := strconv.Atoi(m[3])
		monStr := strings.ToLower(m[2])
		if mon, ok := monthMap[monStr]; ok {
			t := time.Date(year, time.Month(mon), day, 0, 0, 0, 0, time.UTC)
			if t.Day() == day {
				return t, t.Format("2006-01-02"), nil
			}
		}
	}
	mdy := regexp.MustCompile(`^([A-Za-z]+)[-\s]+(\d{1,2})[-\s]+(\d{4})$`)
	if m := mdy.FindStringSubmatch(dateStr); m != nil {
		monStr := strings.ToLower(m[1])
		day, _ := strconv.Atoi(m[2])
		year, _ := strconv.Atoi(m[3])
		if mon, ok := monthMap[monStr]; ok {
			t := time.Date(year, time.Month(mon), day, 0, 0, 0, 0, time.UTC)
			if t.Day() == day {
				return t, t.Format("2006-01-02"), nil
			}
		}
	}
	ymd := regexp.MustCompile(`^(\d{4})[-\s]+([A-Za-z]+)[-\s]+(\d{1,2})$`)
	if m := ymd.FindStringSubmatch(dateStr); m != nil {
		year, _ := strconv.Atoi(m[1])
		monStr := strings.ToLower(m[2])
		day, _ := strconv.Atoi(m[3])
		if mon, ok := monthMap[monStr]; ok {
			t := time.Date(year, time.Month(mon), day, 0, 0, 0, 0, time.UTC)
			if t.Day() == day {
				return t, t.Format("2006-01-02"), nil
			}
		}
	}

	lower := strings.ToLower(dateStr)
	now := time.Now().UTC()
	switch lower {
	case "today":
		t := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
		return t, t.Format("2006-01-02"), nil
	case "yesterday":
		t := now.AddDate(0, 0, -1)
		t = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
		return t, t.Format("2006-01-02"), nil
	case "tomorrow":
		t := now.AddDate(0, 0, 1)
		t = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
		return t, t.Format("2006-01-02"), nil
	}

	daysAgo := regexp.MustCompile(`^(\d+)\s+days?\s+ago$`)
	if m := daysAgo.FindStringSubmatch(lower); m != nil {
		days, _ := strconv.Atoi(m[1])
		t := now.AddDate(0, 0, -days)
		t = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
		return t, t.Format("2006-01-02"), nil
	}

	return time.Time{}, "", fmt.Errorf("unable to parse date: %s", dateStr)
}

func buildDateFilters(before, after, on, during string) (map[string]string, error) {
	out := make(map[string]string)
	if on != "" {
		if during != "" || before != "" || after != "" {
			return nil, fmt.Errorf("'on' cannot be combined with other date filters")
		}
		_, normalized, err := parseFlexibleDate(on)
		if err != nil {
			return nil, fmt.Errorf("invalid 'on' date: %v", err)
		}
		out["on"] = normalized
		return out, nil
	}
	if during != "" {
		if before != "" || after != "" {
			return nil, fmt.Errorf("'during' cannot be combined with 'before' or 'after'")
		}
		_, normalized, err := parseFlexibleDate(during)
		if err != nil {
			return nil, fmt.Errorf("invalid 'during' date: %v", err)
		}
		out["during"] = normalized
		return out, nil
	}
	if after != "" {
		_, normalized, err := parseFlexibleDate(after)
		if err != nil {
			return nil, fmt.Errorf("invalid 'after' date: %v", err)
		}
		out["after"] = normalized
	}
	if before != "" {
		_, normalized, err := parseFlexibleDate(before)
		if err != nil {
			return nil, fmt.Errorf("invalid 'before' date: %v", err)
		}
		out["before"] = normalized
	}
	if after != "" && before != "" {
		a, _, _ := parseFlexibleDate(after)
		b, _, _ := parseFlexibleDate(before)
		if a.After(b) {
			return nil, fmt.Errorf("'after' date is after 'before' date")
		}
	}
	return out, nil
}

func isFilterKey(key string) bool {
	_, ok := validFilterKeys[strings.ToLower(key)]
	return ok
}

func splitQuery(q string) (freeText []string, filters map[string][]string) {
	filters = make(map[string][]string)
	for _, tok := range strings.Fields(q) {
		parts := strings.SplitN(tok, ":", 2)
		if len(parts) == 2 && isFilterKey(parts[0]) {
			key := strings.ToLower(parts[0])
			filters[key] = append(filters[key], parts[1])
		} else {
			freeText = append(freeText, tok)
		}
	}
	return
}

func addFilter(filters map[string][]string, key, val string) {
	for _, existing := range filters[key] {
		if existing == val {
			return
		}
	}
	filters[key] = append(filters[key], val)
}

func buildQuery(freeText []string, filters map[string][]string) string {
	var out []string
	out = append(out, freeText...)
	for _, key := range []string{"is", "in", "from", "with", "before", "after", "on", "during"} {
		for _, val := range filters[key] {
			out = append(out, fmt.Sprintf("%s:%s", key, val))
		}
	}
	return strings.Join(out, " ")
}

func hasImageBlocks(blocks slack.Blocks) bool {
	for _, block := range blocks.BlockSet {
		if block.BlockType() == slack.MBTImage {
			return true
		}
	}
	return false
}
