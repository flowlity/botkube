package bot

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/bwmarrin/discordgo"
	"github.com/sirupsen/logrus"

	"github.com/kubeshop/botkube/pkg/config"
	"github.com/kubeshop/botkube/pkg/events"
	"github.com/kubeshop/botkube/pkg/execute"
	"github.com/kubeshop/botkube/pkg/format"
	"github.com/kubeshop/botkube/pkg/multierror"
	"github.com/kubeshop/botkube/pkg/sliceutil"
)

// TODO: Refactor this file as a part of https://github.com/kubeshop/botkube/issues/667
//    - handle and send methods from `discordMessage` should be defined on Bot level,
//    - split to multiple files in a separate package,
//    - review all the methods and see if they can be simplified.

var _ Bot = &Discord{}

// customTimeFormat holds custom time format string.
const (
	customTimeFormat = "2006-01-02T15:04:05Z"

	// discordBotMentionRegexFmt supports also nicknames (the exclamation mark).
	// Read more: https://discordjs.guide/miscellaneous/parsing-mention-arguments.html#how-discord-mentions-work
	discordBotMentionRegexFmt = "^<@!?%s>"
)

var embedColor = map[config.Level]int{
	config.Info:     8311585,  // green
	config.Warn:     16312092, // yellow
	config.Debug:    8311585,  // green
	config.Error:    13632027, // red
	config.Critical: 13632027, // red
}

// Discord listens for user's message, execute commands and sends back the response.
type Discord struct {
	log             logrus.FieldLogger
	executorFactory ExecutorFactory
	reporter        AnalyticsReporter
	api             *discordgo.Session
	notification    config.Notification
	botID           string
	channelsMutex   sync.RWMutex
	channels        map[string]channelConfigByID
	notifyMutex     sync.Mutex
	botMentionRegex *regexp.Regexp
}

// discordMessage contains message details to execute command and send back the result.
type discordMessage struct {
	log             logrus.FieldLogger
	executorFactory ExecutorFactory

	Event         *discordgo.MessageCreate
	Request       string
	Response      string
	IsAuthChannel bool
	Session       *discordgo.Session
}

// NewDiscord creates a new Discord instance.
func NewDiscord(log logrus.FieldLogger, cfg config.Discord, executorFactory ExecutorFactory, reporter AnalyticsReporter) (*Discord, error) {
	botMentionRegex, err := discordBotMentionRegex(cfg.BotID)
	if err != nil {
		return nil, err
	}

	api, err := discordgo.New("Bot " + cfg.Token)
	if err != nil {
		return nil, fmt.Errorf("while creating Discord session: %w", err)
	}

	channelsCfg := discordChannelsConfigFrom(cfg.Channels)

	return &Discord{
		log:             log,
		reporter:        reporter,
		executorFactory: executorFactory,
		api:             api,
		botID:           cfg.BotID,
		notification:    cfg.Notification,
		channels:        channelsCfg,
		botMentionRegex: botMentionRegex,
	}, nil
}

// Start starts the Discord websocket connection and listens for messages.
func (b *Discord) Start(ctx context.Context) error {
	b.log.Info("Starting bot")

	// Register the messageCreate func as a callback for MessageCreate events.
	b.api.AddHandler(func(s *discordgo.Session, m *discordgo.MessageCreate) {
		dm := discordMessage{
			log:             b.log,
			executorFactory: b.executorFactory,
			Event:           m,
			Session:         s,
		}

		dm.HandleMessage(b)
	})

	// Open a websocket connection to Discord and begin listening.
	err := b.api.Open()
	if err != nil {
		return fmt.Errorf("while opening connection: %w", err)
	}

	err = b.reporter.ReportBotEnabled(b.IntegrationName())
	if err != nil {
		return fmt.Errorf("while reporting analytics: %w", err)
	}

	b.log.Info("BotKube connected to Discord!")

	<-ctx.Done()
	b.log.Info("Shutdown requested. Finishing...")
	err = b.api.Close()
	if err != nil {
		return fmt.Errorf("while closing connection: %w", err)
	}

	return nil
}

// SendEvent sends event notification to Discord ChannelID.
// Context is not supported by client: See https://github.com/bwmarrin/discordgo/issues/752.
func (b *Discord) SendEvent(_ context.Context, event events.Event, eventSources []string) (err error) {
	b.log.Debugf(">> Sending to Discord: %+v", event)

	msgToSend := b.formatMessage(event)

	errs := multierror.New()
	for _, channelID := range b.getChannelsToNotify(eventSources) {
		msg := msgToSend // copy as the struct is modified when using Discord API client
		if _, err := b.api.ChannelMessageSendComplex(channelID, &msg); err != nil {
			errs = multierror.Append(errs, fmt.Errorf("while sending Discord message to channel %q: %w", channelID, err))
			continue
		}

		b.log.Debugf("Event successfully sent to channel %q", channelID)
	}

	return errs.ErrorOrNil()
}

// SendMessage sends message to Discord channel.
// Context is not supported by client: See https://github.com/bwmarrin/discordgo/issues/752.
func (b *Discord) SendMessage(_ context.Context, msg string) error {
	errs := multierror.New()
	for _, channel := range b.getChannels() {
		channelID := channel.ID
		b.log.Debugf(">> Sending message to channel %q: %+v", channelID, msg)
		if _, err := b.api.ChannelMessageSend(channelID, msg); err != nil {
			errs = multierror.Append(errs, fmt.Errorf("while sending Discord message to channel %q: %w", channelID, err))
			continue
		}
		b.log.Debugf("Message successfully sent to channel %q", channelID)
	}

	return errs.ErrorOrNil()
}

// IntegrationName describes the integration name.
func (b *Discord) IntegrationName() config.CommPlatformIntegration {
	return config.DiscordCommPlatformIntegration
}

// Type describes the integration type.
func (b *Discord) Type() config.IntegrationType {
	return config.BotIntegrationType
}

// TODO: Support custom routing via annotations for Discord as well
func (b *Discord) getChannelsToNotify(eventSources []string) []string {
	var out []string
	for _, cfg := range b.getChannels() {
		switch {
		case !cfg.notify:
			b.log.Info("Skipping notification for channel %q as notifications are disabled.", cfg.Identifier())
		default:
			if sliceutil.Intersect(eventSources, cfg.Bindings.Sources) {
				out = append(out, cfg.Identifier())
			}
		}
	}
	return out
}

// NotificationsEnabled returns current notification status for a given channel ID.
func (b *Discord) NotificationsEnabled(channelID string) bool {
	channel, exists := b.getChannels()[channelID]
	if !exists {
		return false
	}

	return channel.notify
}

// SetNotificationsEnabled sets a new notification status for a given channel ID.
func (b *Discord) SetNotificationsEnabled(channelID string, enabled bool) error {
	// avoid race conditions with using the setter concurrently, as we set whole map
	b.notifyMutex.Lock()
	defer b.notifyMutex.Unlock()

	channels := b.getChannels()
	channel, exists := channels[channelID]
	if !exists {
		return execute.ErrNotificationsNotConfigured
	}

	channel.notify = enabled
	channels[channelID] = channel
	b.setChannels(channels)

	return nil
}

// HandleMessage handles the incoming messages.
func (dm *discordMessage) HandleMessage(b *Discord) {
	// Handle message only if starts with mention
	trimmedMsg, found := b.findAndTrimBotMention(dm.Event.Content)
	if !found {
		b.log.Debugf("Ignoring message as it doesn't contain %q mention", b.botID)
		return
	}
	dm.Request = trimmedMsg

	channel, exists := b.getChannels()[dm.Event.ChannelID]
	dm.IsAuthChannel = exists

	e := dm.executorFactory.NewDefault(b.IntegrationName(), b, dm.IsAuthChannel, channel.Identifier(), channel.Bindings.Executors, dm.Request)

	dm.Response = e.Execute()
	dm.Send()
}

func (dm *discordMessage) Send() {
	dm.log.Debugf("Discord incoming Request: %s", dm.Request)
	dm.log.Debugf("Discord Response: %s", dm.Response)

	if len(dm.Response) == 0 {
		dm.log.Errorf("Invalid request. Dumping the response. Request: %s", dm.Request)
		return
	}

	// Upload message as a file if too long
	if len(dm.Response) >= 2000 {
		params := &discordgo.MessageSend{
			Content: dm.Request,
			Files: []*discordgo.File{
				{
					Name:   "Response",
					Reader: strings.NewReader(dm.Response),
				},
			},
		}
		if _, err := dm.Session.ChannelMessageSendComplex(dm.Event.ChannelID, params); err != nil {
			dm.log.Error("Error in uploading file:", err)
		}
		return
	}

	if _, err := dm.Session.ChannelMessageSend(dm.Event.ChannelID, format.CodeBlock(dm.Response)); err != nil {
		dm.log.Error("Error in sending message:", err)
	}
}

func (b *Discord) getChannels() map[string]channelConfigByID {
	b.channelsMutex.RLock()
	defer b.channelsMutex.RUnlock()
	return b.channels
}

func (b *Discord) setChannels(channels map[string]channelConfigByID) {
	b.channelsMutex.Lock()
	defer b.channelsMutex.Unlock()
	b.channels = channels
}

func (b *Discord) findAndTrimBotMention(msg string) (string, bool) {
	if !b.botMentionRegex.MatchString(msg) {
		return "", false
	}

	return b.botMentionRegex.ReplaceAllString(msg, ""), true
}

func discordChannelsConfigFrom(channelsCfg config.IdentifiableMap[config.ChannelBindingsByID]) map[string]channelConfigByID {
	res := make(map[string]channelConfigByID)
	for _, channCfg := range channelsCfg {
		res[channCfg.Identifier()] = channelConfigByID{
			ChannelBindingsByID: channCfg,
			notify:              defaultNotifyValue,
		}
	}

	return res
}

func discordBotMentionRegex(botID string) (*regexp.Regexp, error) {
	botMentionRegex, err := regexp.Compile(fmt.Sprintf(discordBotMentionRegexFmt, botID))
	if err != nil {
		return nil, fmt.Errorf("while compiling bot mention regex: %w", err)
	}

	return botMentionRegex, nil
}
