package service

import (
	"errors"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nlopes/slack"

	"github.com/j-martin/slag/components"
)

type SlackService struct {
	Client          *slack.Client
	RTM             *slack.RTM
	Conversations   []slack.Channel
	UserCache       map[string]string
	CurrentUserID   string
	CurrentUsername string
	CurrentTeamInfo *slack.TeamInfo
	Channels        map[string]components.Channel
	mutex           *sync.Mutex
}

// NewSlackService is the constructor for the SlackService and will initialize
// the RTM and a Client
func NewSlackService(token string) (*SlackService, error) {
	svc := &SlackService{
		Client:    slack.New(token),
		UserCache: make(map[string]string),
		mutex:     &sync.Mutex{},
	}

	// Get user associated with token, mainly
	// used to identify user when new messages
	// arrives
	authTest, err := svc.Client.AuthTest()
	if err != nil {
		return nil, errors.New("not able to authorize client, check your connection and if your slack-token is set correctly")
	}
	svc.CurrentUserID = authTest.UserID

	// Create RTM
	svc.RTM = svc.Client.NewRTM()
	go svc.RTM.ManageConnection()

	// Creation of user cache this speeds up
	// the uncovering of usernames of messages
	users, _ := svc.Client.GetUsers()
	for _, user := range users {
		// only add non-deleted users
		if !user.Deleted {
			svc.setCachedUser(user.ID, user.Name)
		}
	}

	teamInfo, err := svc.GetTeamInfo()
	if err != nil {
		return nil, err
	}
	svc.CurrentTeamInfo = teamInfo
	// Get name of current user
	currentUser, err := svc.Client.GetUserInfo(svc.CurrentUserID)
	if err != nil {
		svc.CurrentUsername = "slag"
	}
	svc.CurrentUsername = currentUser.Name

	return svc, nil
}

func (s *SlackService) GetTeamInfo() (*slack.TeamInfo, error) {
	return s.Client.GetTeamInfo()
}

func (s *SlackService) GetChannels() ([]components.Channel, error) {
	slackChans := make([]slack.Channel, 0)

	// Initial request
	initChans, initCur, err := s.Client.GetConversations(
		&slack.GetConversationsParameters{
			ExcludeArchived: "true",
			Limit:           1000,
			Types: []string{
				"public_channel",
				"private_channel",
				"im",
				"mpim",
			},
		},
	)
	if err != nil {
		return nil, err
	}

	slackChans = append(slackChans, initChans...)

	// Paginate over additional channels
	nextCur := initCur
	for nextCur != "" {
		channels, cursor, err := s.Client.GetConversations(
			&slack.GetConversationsParameters{
				Cursor:          nextCur,
				ExcludeArchived: "true",
				Limit:           1000,
				Types: []string{
					"public_channel",
					"private_channel",
					"im",
					"mpim",
				},
			},
		)
		if err != nil {
			return nil, err
		}

		slackChans = append(slackChans, channels...)
		nextCur = cursor
	}

	// We're creating tempChan, because we want to be able to
	// sort the types of channels into buckets
	type tempChan struct {
		channelItem  components.Channel
		slackChannel slack.Channel
	}

	// Initialize buckets
	buckets := make(map[int]map[string]*tempChan)
	buckets[0] = make(map[string]*tempChan) // Channels
	buckets[1] = make(map[string]*tempChan) // Group
	buckets[2] = make(map[string]*tempChan) // MpIM
	buckets[3] = make(map[string]*tempChan) // IM

	var wg sync.WaitGroup
	for _, chn := range slackChans {
		chanItem := s.createChannelItem(chn)

		if chn.IsChannel {
			if !chn.IsMember {
				continue
			}

			buckets[0][chn.ID] = &tempChan{
				channelItem:  chanItem,
				slackChannel: chn,
			}
		}

		if chn.IsGroup {
			if !chn.IsMember {
				continue
			}

			// This is done because MpIM channels are also considered groups
			if chn.IsMpIM {
				if !chn.IsOpen {
					continue
				}

				buckets[2][chn.ID] = &tempChan{
					channelItem:  chanItem,
					slackChannel: chn,
				}
			} else {

				buckets[1][chn.ID] = &tempChan{
					channelItem:  chanItem,
					slackChannel: chn,
				}
			}
		}

		if chn.IsIM {
			// Check if user is deleted, we do this by checking the user id,
			// and see if we have the user in the UserCache
			name, ok := s.getCachedUser(chn.User)
			if !ok {
				continue
			}

			chanItem.Name = name
			buckets[3][chn.User] = &tempChan{
				channelItem:  chanItem,
				slackChannel: chn,
			}

			wg.Add(1)
			go func(user string, buckets map[int]map[string]*tempChan) {
				defer wg.Done()

				presence, err := s.GetUserPresence(user)
				if err != nil {
					buckets[3][user].channelItem.Presence = "away"
					return
				}

				buckets[3][user].channelItem.Presence = presence
			}(chn.User, buckets)
		}
	}

	wg.Wait()

	// Sort the buckets
	var keys []int
	for k := range buckets {
		keys = append(keys, k)
	}
	sort.Ints(keys)

	var chans []components.Channel
	for _, k := range keys {

		bucket := buckets[k]

		// Sort channels in every bucket
		tcArr := make([]tempChan, 0)
		for _, v := range bucket {
			tcArr = append(tcArr, *v)
		}

		sort.Slice(tcArr, func(i, j int) bool {
			return tcArr[i].channelItem.Name < tcArr[j].channelItem.Name
		})

		// Add Channel and SlackChannel to the SlackService struct
		for _, tc := range tcArr {
			chans = append(chans, tc.channelItem)
			s.Conversations = append(s.Conversations, tc.slackChannel)
		}
	}

	return chans, nil
}

// GetUserPresence will get the presence of a specific user
func (s *SlackService) GetUserPresence(userID string) (string, error) {
	presence, err := s.Client.GetUserPresence(userID)
	if err != nil {
		return "", err
	}

	return presence.Presence, nil
}

// MarkAsRead will set the channel as read
func (s *SlackService) MarkAsRead(channelID string) {
	s.Client.SetChannelReadMark(
		channelID, fmt.Sprintf("%f",
			float64(time.Now().Unix())),
	)
}

// GetMessages will get messages for a channel, group or im channel delimited
// by a count.
func (s *SlackService) GetMessages(channel components.Channel, count int) ([]components.Message, error) {

	// https://godoc.org/github.com/nlopes/slack#GetConversationHistoryParameters
	historyParams := slack.GetConversationHistoryParameters{
		ChannelID: channel.ID,
		Limit:     count,
		Inclusive: false,
	}

	history, err := s.Client.GetConversationHistory(&historyParams)
	if err != nil {
		return nil, err
	}

	// Construct the messages
	var messages []components.Message
	for _, message := range history.Messages {
		msg, err := s.CreateMessage(message, &channel)
		if err != nil {
			return nil, err
		}
		messages = append(messages, msg...)
	}
	return messages, nil
}

// CreateMessage will create a string formatted message that can be rendered
// in the Chat pane.
//
// [23:59] <erroneousboat> Hello world!
//
// This returns an array of string because we will try to uncover attachments
// associated with messages.
func (s *SlackService) CreateMessage(message slack.Message, channel *components.Channel) ([]components.Message, error) {
	var msgs []components.Message
	var name string

	// Get username from cache
	User := message.User
	name, ok := s.getCachedUser(User)

	// Name not in cache
	if !ok {
		if message.BotID != "" {
			// Name not found, perhaps a bot, use Username
			name, ok = s.getCachedUser(message.BotID)
			if !ok {
				// Not found in cache, add it
				name = message.Username
				s.setCachedUser(message.BotID, message.Username)
			}
		} else {
			// Not a bot, not in cache, get user info
			user, err := s.Client.GetUserInfo(User)
			if err != nil {
				name = "unknown"
				s.setCachedUser(User, name)
			} else {
				name = user.Name
				s.setCachedUser(User, user.Name)
			}
		}
	}

	if name == "" {
		name = "unknown"
	}

	// When there are attachments append them
	threadTimestamp := message.ThreadTimestamp
	if threadTimestamp == "" {
		threadTimestamp = message.Timestamp
	}
	msg := components.Message{
		ThreadTimestamp: threadTimestamp,
		Channel:         channel,
		Time:            parseTime(message),
		Name:            name,
		Content:         parseMessage(s, message.Text),
		Attachments:     s.FormatAttachments(message.Attachments, message.Files),
		IsReply:         message.ThreadTimestamp != "",
	}

	msgs = append(msgs, msg)

	if len(message.Replies) > 0 {
		replies, err := s.CreateMessageFromReplies(&message, channel)
		if err != nil {
			return nil, err
		}
		msgs = append(msgs, replies...)
	}

	return msgs, nil
}

func (s *SlackService) getCachedUser(ID string) (string, bool) {
	defer s.mutex.Unlock()
	s.mutex.Lock()
	i, ok := s.UserCache[ID]
	return i, ok
}

func (s *SlackService) setCachedUser(ID string, Username string) {
	defer s.mutex.Unlock()
	s.mutex.Lock()
	s.UserCache[ID] = Username
}

func parseTime(message slack.Message) time.Time {
	// Parse time
	floatTime, err := strconv.ParseFloat(message.Timestamp, 64)
	if err != nil {
		floatTime = 0.0
	}
	intTime := int64(floatTime)
	// Format message
	msgTime := time.Unix(intTime, 0)
	return msgTime
}

// CreateMessageFromReplies will create components.Message struct from
// the conversation replies from slack.
//
// Useful documentation:
//
// https://api.slack.com/docs/message-threading
// https://api.slack.com/methods/conversations.replies
// https://godoc.org/github.com/nlopes/slack#Client.GetConversationReplies
// https://godoc.org/github.com/nlopes/slack#GetConversationRepliesParameters
func (s *SlackService) CreateMessageFromReplies(parentMessage *slack.Message, channel *components.Channel) ([]components.Message, error) {
	msgs := make([]slack.Message, 0)

	initReplies, _, initCur, err := s.Client.GetConversationReplies(
		&slack.GetConversationRepliesParameters{
			ChannelID: channel.ID,
			Timestamp: parentMessage.ThreadTimestamp,
			Limit:     200,
		},
	)
	if err != nil {
		log.Fatal(err) // FIXME
	}

	msgs = append(msgs, initReplies...)

	nextCur := initCur
	for nextCur != "" {
		conversationReplies, _, cursor, err := s.Client.GetConversationReplies(&slack.GetConversationRepliesParameters{
			ChannelID: channel.ID,
			Timestamp: parentMessage.ThreadTimestamp,
			Cursor:    nextCur,
			Limit:     200,
		})

		if err != nil {
			log.Fatal(err) // FIXME
		}

		msgs = append(msgs, conversationReplies...)
		nextCur = cursor
	}

	var replies []components.Message
	for _, reply := range msgs {

		// Because the conversations api returns an entire thread (a
		// message plus all the messages in reply), we need to check if
		// one of the replies isn't the parent that we started with.
		//
		// Keep in mind that the api returns the replies with the latest
		// as the first element.
		if reply.ThreadTimestamp != "" && reply.ThreadTimestamp == reply.Timestamp {
			continue
		}

		msg, err := s.CreateMessage(reply, channel)
		if err != nil {
			return nil, err
		}
		replies = append(replies, msg...)
	}

	return replies, nil
}

func (s *SlackService) ListenToEvents(watchChannels map[string]*components.Channel, printer func(components.Message, *slack.TeamInfo)) error {
	for msg := range s.RTM.IncomingEvents {
		switch ev := msg.Data.(type) {
		case *slack.HelloEvent:
			// Ignore hello

		case *slack.MessageEvent:
			channel := watchChannels[ev.Channel]
			if channel == nil {
				continue
			}
			messages, err := s.CreateMessageFromMessageEvent(channel, ev)
			if err != nil {
				return err
			}
			for _, message := range messages {
				printer(message, s.CurrentTeamInfo)
			}

		case *slack.RTMError:
			msg := fmt.Sprintf("Error: %s\n", ev.Error())
			return errors.New(msg)

		case *slack.InvalidAuthEvent:
			msg := "Invalid credentials"
			return errors.New(msg)

		default:
			//fmt.Printf("%v\n", ev)
		}
	}
	return nil
}

func (s *SlackService) CreateMessageFromMessageEvent(channel *components.Channel, message *slack.MessageEvent) ([]components.Message, error) {

	var msgs []components.Message
	var name string

	switch message.SubType {
	case "message_changed":
		// Append (edited) when an edited message is received
		message = &slack.MessageEvent{Msg: *message.SubMessage}
		message.Text = fmt.Sprintf("%s (edited)", message.Text)
	case "message_replied":
		// Ignore reply events
		return nil, nil
	}

	// Get username from cache
	name, ok := s.getCachedUser(message.User)

	// Name not in cache
	if !ok {
		if message.BotID != "" {
			// Name not found, perhaps a bot, use Username
			name, ok = s.getCachedUser(message.BotID)
			if !ok {
				// Not found in cache, add it
				name = message.Username
				s.setCachedUser(message.BotID, message.Username)
			}
		} else {
			// Not a bot, not in cache, get user info
			user, err := s.Client.GetUserInfo(message.User)
			if err != nil {
				name = "unknown"
				s.setCachedUser(message.User, name)
			} else {
				name = user.Name
				s.setCachedUser(message.User, user.Name)
			}
		}
	}

	if name == "" {
		name = "unknown"
	}

	// Parse time
	floatTime, err := strconv.ParseFloat(message.Timestamp, 64)
	if err != nil {
		floatTime = 0.0
	}
	intTime := int64(floatTime)

	// Format message
	threadTimestamp := message.ThreadTimestamp
	if threadTimestamp == "" {
		threadTimestamp = message.Timestamp
	}
	msg := components.Message{
		Channel:         channel,
		ThreadTimestamp: threadTimestamp,
		Time:            time.Unix(intTime, 0),
		Name:            name,
		Content:         parseMessage(s, message.Text),
		Attachments:     s.FormatAttachments(message.Attachments, message.Files),
	}

	msgs = append(msgs, msg)

	return msgs, nil
}

func parseMessage(s *SlackService, msg string) string {
	msg = parseEmoji(msg)
	msg = parseMentions(s, msg)
	return msg
}

// parseMentions will try to find mention placeholders in the message
// string and replace them with the correct username with and @ symbol
//
// Mentions have the following format:
//	<@U12345|erroneousboat>
//		<@U12345>
func parseMentions(s *SlackService, msg string) string {
	r := regexp.MustCompile(`\<@(\w+\|*\w+)\>`)

	return r.ReplaceAllStringFunc(
		msg, func(str string) string {
			rs := r.FindStringSubmatch(str)
			if len(rs) < 1 {
				return str
			}

			var userID string
			split := strings.Split(rs[1], "|")
			if len(split) > 0 {
				userID = split[0]
			} else {
				userID = rs[1]
			}

			name, ok := s.getCachedUser(userID)
			if !ok {
				user, err := s.Client.GetUserInfo(userID)
				if err != nil {
					name = "unknown"
					s.setCachedUser(userID, name)
				} else {
					name = user.Name
					s.setCachedUser(userID, user.Name)
				}
			}

			if name == "" {
				name = "unknown"
			}

			return "@" + name
		},
	)
}

// parseEmoji will try to find emoji placeholders in the message
// string and replace them with the correct unicode equivalent
func parseEmoji(msg string) string {
	r := regexp.MustCompile("(:\\w+:)")

	return r.ReplaceAllStringFunc(
		msg, func(str string) string {
			code, ok := EmojiCodemap[str]
			if !ok {
				return str
			}
			return code
		},
	)
}

// FormatAttachments will construct a array of string of the Field
// values of Attachments from a Message.
func (s *SlackService) FormatAttachments(attachments []slack.Attachment, files []slack.File) []components.Attachment {
	var finalAttachments []components.Attachment
	for _, attachment := range attachments {
		if attachment.AuthorName != "" {
			finalAttachments = append(
				finalAttachments,
				components.Attachment{Content: attachment.AuthorName, Type: "other"},
			)
		}

		if attachment.Title != "" {
			finalAttachments = append(
				finalAttachments,
				components.Attachment{Content: SanitizeLinks(attachment.Title), Type: "title"},
			)
		}

		if attachment.TitleLink != "" {
			finalAttachments = append(
				finalAttachments,
				components.Attachment{Content: SanitizeLinks(attachment.TitleLink), Type: "link"},
			)
		}

		if attachment.Text != "" {
			finalAttachments = append(
				finalAttachments,
				components.Attachment{Content: SanitizeLinks(attachment.Text), Type: "text"},
			)
		}

		for i := len(attachment.Fields) - 1; i >= 0; i-- {
			finalAttachments = append(finalAttachments,
				components.Attachment{Content: fmt.Sprintf(
					"%s %s",
					attachment.Fields[i].Title,
					attachment.Fields[i].Value,
				), Type: "link"},
			)
		}
	}
	for _, file := range files {
		finalAttachments = append(finalAttachments,
			components.Attachment{Content: fmt.Sprintf("%s ⇒ %s", file.Name, file.URLPrivate), Type: "link"})
		if file.Preview != "" {
			finalAttachments = append(finalAttachments,
				components.Attachment{Content: file.Preview, Type: "link"})
		}
	}

	return finalAttachments
}

var regex *regexp.Regexp

func init() {
	var err error
	regex, err = regexp.Compile("(<)(https://.*?)([|>])")
	if err != nil {
		log.Fatal("Failed to init regex")
	}
}
func SanitizeLinks(message string) string {
	return regex.ReplaceAllString(message, "$1 $2 $3")
}

func (s *SlackService) createChannelItem(chn slack.Channel) components.Channel {
	return components.Channel{
		ID:     chn.ID,
		Name:   chn.Name,
		Topic:  chn.Topic.Value,
		UserID: chn.User,
	}
}
