// Copyright 2017 Tamás Gulácsi. All rights reserved.

package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/pkg/errors"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"

	"gopkg.in/telegram-bot-api.v4"
)

//go:generate protoc --gofast_out=plugins=grpc:. ./pb/tbot.proto

var timeout = 15 * time.Second

func main() {
	if err := Main(); err != nil {
		log.Fatal(err)
	}
}

func Main() error {

	mainCmd := &cobra.Command{Use: "tbot"}
	var addr, token string

	sendMsgCmd := &cobra.Command{
		Use:     "send",
		Aliases: []string{"message", "msg", "m", "s"},
		RunE: func(_ *cobra.Command, args []string) error {
			u := addr + "/message/" + args[0]
			resp, err := http.DefaultClient.Post(u, "text/plain", strings.NewReader(strings.Join(args[1:], " ")))
			if err != nil {
				return errors.Wrap(err, u)
			}
			defer resp.Body.Close()
			log.Println(resp.Status)
			io.Copy(os.Stdout, resp.Body)
			return nil
		},
	}
	sendMsgCmd.Flags().StringVar(&addr, "upstream", "http://unowebprd:23456", "address of agent or proxy")
	mainCmd.AddCommand(sendMsgCmd)

	getBot := func(debug bool) tgBot {
		if token == "" {
			log.Fatal("You have to set environment variable TELEGRAM_TOKEN first!")
		}
		ba, err := tgbotapi.NewBotAPI(token)
		if err != nil {
			log.Fatalf("Error creating bot: %v", err)
		}
		ba.Debug = debug
		bot := tgBot{BotAPI: ba}
		log.Printf("Bot started with %q.", bot.Self.UserName)
		return bot
	}

	var dataDir string
	var debug bool
	sendDirectCmd := &cobra.Command{
		Use:     "direct",
		Aliases: []string{"send-direct"},
		RunE: func(_ *cobra.Command, args []string) error {
			log.Println("Opening " + dataDir)
			data, err := newDataPath(dataDir)
			if err != nil {
				return err
			}
			if err := data.Load(); err != nil {
				log.Println(err)
			}
			to := args[0]
			if u := data.usersMap[to]; u != nil && u.LastChatID != 0 {
				bot := getBot(debug)
				return bot.Send(fmt.Sprintf("%d", u.LastChatID), strings.Join(args[1:], " "))
			}
			return errors.New("No chat ID for " + to)
		},
	}
	sendDirectCmd.Flags().StringVar(&token, "token", os.Getenv("TELEGRAM_TOKEN"), "telegram token")
	sendDirectCmd.Flags().StringVarP(&dataDir, "data", "d", ".", "path for data")
	sendDirectCmd.Flags().BoolVarP(&debug, "verbose", "v", false, "verbose logging")
	mainCmd.AddCommand(sendDirectCmd)

	serveCmd := &cobra.Command{
		Use:     "serve",
		Aliases: []string{"server", "srv", "proxy"},
		RunE: func(_ *cobra.Command, args []string) error {
			if len(args) < 1 {
				return errors.New("address is needed to listen on")
			}
			bot := getBot(debug)
			data, err := newDataPath(dataDir)
			if err != nil {
				return err
			}
			if err := data.Load(); err != nil {
				log.Println(err)
			}
			s := srv{dataPath: data, bot: bot, agents: make(map[string]string, 8)}
			http.Handle("/", s)
			var grp errgroup.Group
			grp.Go(func() error {
				log.Println("Listening on " + args[0])
				return http.ListenAndServe(args[0], nil)
			})
			grp.Go(s.Run)
			return grp.Wait()
		},
	}
	serveCmd.Flags().StringVar(&token, "token", os.Getenv("TELEGRAM_TOKEN"), "telegram token")
	serveCmd.Flags().StringVarP(&dataDir, "data", "d", ".", "path for data")
	serveCmd.Flags().BoolVarP(&debug, "verbose", "v", false, "verbose logging")
	mainCmd.AddCommand(serveCmd)

	var upstream, name, baseDir string
	agentCmd := &cobra.Command{
		Use: "agent",
		RunE: func(_ *cobra.Command, args []string) error {
			if len(args) < 1 {
				return errors.New("address is needed to listen on")
			}
			return agent{upstream: upstream, name: name, baseDir: baseDir}.Run(args[0])
		},
	}
	agentCmd.Flags().StringVarP(&upstream, "upstream", "u", "http://unowebprd.unosoft.local:23456", "upstream address")
	agentCmd.Flags().StringVarP(&name, "name", "n", os.ExpandEnv("${BRUNO_CUS}_${BRUNO_ENV}"), "upstream address")
	agentCmd.Flags().StringVarP(&baseDir, "base-dir", "d", os.ExpandEnv("${BRUNO_HOME}/../admin/bot"), "path of command files")
	mainCmd.AddCommand(agentCmd)

	return mainCmd.Execute()
}

type tgBot struct {
	*tgbotapi.BotAPI
}

func (bot tgBot) Send(chatID string, text string) error {
	_, err := bot.BotAPI.Send(tgbotapi.NewMessageToChannel(chatID, text))
	return err
}

func (bot tgBot) Reply(msg *tgbotapi.Message, text string) (tgbotapi.Message, error) {
	reply := tgbotapi.NewMessage(msg.Chat.ID, text)
	reply.ReplyToMessageID = msg.MessageID
	return bot.BotAPI.Send(reply)
}

func (bot tgBot) NewUpdate(offset, timeout int) tgbotapi.UpdateConfig {
	u := tgbotapi.NewUpdate(offset)
	u.Timeout = timeout
	return u
}

type tgMsg struct {
	*tgbotapi.Message
	bot *tgBot
}

func (msg tgMsg) Reply(text string) error {
	reply := tgbotapi.NewMessage(msg.Chat.ID, text)
	reply.ReplyToMessageID = msg.MessageID
	_, err := msg.bot.BotAPI.Send(reply)
	return err
}

func (msg tgMsg) IsGroup() bool { return msg.Message.Chat.IsGroup() }

// vim: set fileencoding=utf-8 noet:
