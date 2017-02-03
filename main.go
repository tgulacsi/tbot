// Copyright 2017 Tamás Gulácsi. All rights reserved.

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/pkg/errors"
	"github.com/spf13/cobra"
	"golang.org/x/net/context"
	"golang.org/x/sync/errgroup"

	"gopkg.in/telegram-bot-api.v4"
)

//go:generate protoc --gofast_out=plugins=grpc:. ./pb/tbot.proto

var (
	token      string
	BotBaseDir string
	Port       = 8684

	timeout = 15 * time.Second
)

func init() {
	token = os.Getenv("TELEGRAM_TOKEN")
	if os.Getenv("BRUNO_HOME") == "" {
		BotBaseDir, _ = os.Getwd()
	} else {
		BotBaseDir = os.ExpandEnv("$BRUNO_HOME/../admin/bot")
	}
}

func main() {
	if err := Main(); err != nil {
		log.Fatal(err)
	}
}

func Main() error {
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

	mainCmd := &cobra.Command{Use: "tbot"}
	var addr string

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
				_, err = bot.Send(tgbotapi.NewMessageToChannel(fmt.Sprintf("%d", u.LastChatID), strings.Join(args[1:], " ")))
				return err
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

	var upstream, name string
	agentCmd := &cobra.Command{
		Use: "agent",
		RunE: func(_ *cobra.Command, args []string) error {
			if len(args) < 1 {
				return errors.New("address is needed to listen on")
			}
			return agent{upstream: upstream, name: name}.Run(args[0])
		},
	}
	agentCmd.Flags().StringVarP(&upstream, "upstream", "u", "http://unowebprd.unosoft.local:23456", "upstream address")
	agentCmd.Flags().StringVarP(&name, "name", "n", os.ExpandEnv("${BRUNO_CUS}_${BRUNO_ENV}"), "upstream address")
	mainCmd.AddCommand(agentCmd)

	return mainCmd.Execute()
}

func (s srv) Run() error {
	for to, texts := range s.queues {
		if u := s.usersMap[to]; u != nil && u.LastChatID != 0 {
			remains := make([]string, 0, len(texts))
			cid := fmt.Sprintf("%d", u.LastChatID)
			for _, text := range texts {
				if _, err := s.bot.Send(tgbotapi.NewMessageToChannel(cid, text)); err != nil {
					remains = append(remains, text)
				}
			}
			s.queues[to] = remains
		}
	}
	if err := s.Save(); err != nil {
		log.Println(err)
	}

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates, err := s.bot.GetUpdatesChan(u)
	if err != nil {
		log.Println(err)
	}
	var buf bytes.Buffer
	for update := range updates {
		msg := update.Message
		if msg == nil {
			continue
		}
		log.Printf("[%s] %s", msg.From.UserName, msg.Text)
		uname := msg.From.UserName
		u := s.usersMap[uname]
		if u == nil {
			u = &User{Name: msg.From.UserName, Aliases: aliases[msg.From.UserName]}
			s.users = append(s.users, *u)
			i := len(s.users) - 1
			s.usersMap[uname] = &s.users[i]
		}
		if u != nil && u.LastChatID != msg.Chat.ID {
			s.usersMap[uname].LastChatID = msg.Chat.ID
			var remains []string
			cid := fmt.Sprintf("%d", msg.Chat.ID)
			for _, text := range s.queues[uname] {
				if _, err = s.bot.Send(tgbotapi.NewMessageToChannel(cid, text)); err != nil {
					remains = append(remains, text)
				}
			}
			s.queues[uname] = remains
			if err := s.Save(); err != nil {
				log.Println(err)
			}
		}

		command, args := msg.Text, []string{}
		if i := strings.IndexByte(command, ' '); i >= 0 {
			command, args = command[:i], strings.Split(command[i+1:], " ")
		}
		if command == "/help" {
			s.bot.Reply(msg,
				`/help Show this message.
/doku	Mi változott, mit kellene dokumentálni?
/oerr	Oracle hibakód kereső
/forward Forward last message.

or you can send message to me, I will reply it with some debug message.`)
		}

		if !strings.HasPrefix(command, "/") || len(args) < 1 {
			s.bot.Reply(msg, "/parancs <gép> [args]")
			continue
		}

		ag := s.agents[args[0]]
		if ag == "" {
			s.bot.Reply(msg,
				fmt.Sprintf("%q nem ismert gép!\nIsmertek: %s", args[0], s.agents))
			continue
		}
		URL := ag + "/" + command + "?" + url.Values{"args": args[1:]}.Encode()
		resp, err := http.Get(URL)
		if err != nil {
			s.bot.Reply(msg, errors.Wrap(err, URL).Error())
			continue
		}
		buf.Reset()
		_, err = io.Copy(&buf, resp.Body)
		resp.Body.Close()
		if err != nil {
			log.Println(err)
		}
		s.bot.Reply(msg, buf.String())
	}
	return nil
}

func (ag agent) execute(bot tgBot, msg *tgbotapi.Message) error {
	if msg.Chat.IsGroup() || msg.Text == "" {
		return nil
	}
	E := func(err error) error {
		log.Printf("%v", err)
		bot.Reply(msg, fmt.Sprintf("%+v", err))
		return err
	}

	command, args := strings.TrimPrefix(msg.Text, "/"), ""
	if i := strings.IndexByte(command, ' '); i >= 0 {
		command, args = command[:i], command[i+1:]
	}
	fn := filepath.Join(BotBaseDir, command+".sh")
	fi, err := os.Stat(fn)
	if err != nil {
		return E(errors.Wrapf(err, fn))
	}
	if fi.Mode().Perm()&0111 == 0 {
		return E(errors.Wrapf(err, "permission=%s", fi.Mode().Perm()))
	}
	env := append(make([]string, 0, len(os.Environ())+1), os.Environ()...)
	env = append(env, "TBOT_SENDER="+msg.From.UserName)
	var buf bytes.Buffer
	cmd := exec.Command("sh", "-c", fn+" "+args)
	cmd.Dir = BotBaseDir
	cmd.Stdout = io.MultiWriter(&buf, os.Stdout)
	cmd.Stderr = cmd.Stdout
	cmd.Env = env
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	log.Printf("calling %q", cmd.Args)
	if err := runWithContext(ctx, cmd); err != nil {
		return E(errors.Wrapf(err, "start %q", cmd.Args))
	}
	_, err = bot.Reply(msg, buf.String())
	return err
}

type tgBot struct {
	*tgbotapi.BotAPI
}

func (bot tgBot) Reply(msg *tgbotapi.Message, text string) (tgbotapi.Message, error) {
	reply := tgbotapi.NewMessage(msg.Chat.ID, text)
	reply.ReplyToMessageID = msg.MessageID
	return bot.BotAPI.Send(reply)
}

func runWithContext(ctx context.Context, cmd *exec.Cmd) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error)
	go func() { done <- cmd.Wait() }()
	select {
	case <-ctx.Done():
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
		return ctx.Err()
	case err := <-done:
		return err
	}
	return nil
}

type srv struct {
	*dataPath
	bot    tgBot
	agents map[string]string
}

func (s srv) Register(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/register/")
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	port := r.URL.Query().Get("port")
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	s.agents[name] = scheme + "://" + host + ":" + port
	log.Printf("Registered %q to %q", name, s.agents[name])
	fmt.Fprintf(w, "registered %q to %q", name, s.agents[name])
}

func (s srv) Message(w http.ResponseWriter, r *http.Request) {
	if r != nil && r.Body != nil {
		defer r.Body.Close()
	}
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 2)
	log.Printf("%s %s (parts=%v)", r.Method, r.URL, parts)
	if len(parts) < 1 {
		if r.Method == "POST" {
			http.Error(w, "POST needs username", http.StatusBadRequest)
			return
		}
		je := json.NewEncoder(w)
		je.Encode(s.users)
		je.Encode(s.queues)
		return
	}

	to := parts[0]
	var text string
	if r.Method == "POST" {
		b, _ := ioutil.ReadAll(r.Body)
		text = string(b)
	} else if len(parts) > 1 {
		text = parts[1]
	}
	if text == "" {
		http.Error(w, "Empty message", http.StatusBadRequest)
		return
	}

	if u := s.usersMap[to]; u != nil && u.LastChatID != 0 {
		log.Println("Sending to " + to)
		_, err := s.bot.Send(tgbotapi.NewMessageToChannel(fmt.Sprintf("%d", u.LastChatID), text))
		if err == nil {
			w.WriteHeader(201)
			return
		}
	}
	s.queues[to] = append(s.queues[to], text)
	if err := s.Save(); err != nil {
		log.Fatal(err)
	}
}

func (s srv) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r != nil && r.Body != nil {
		defer r.Body.Close()
	}
	if r != nil && r.Method == "PUT" && strings.HasPrefix(r.URL.Path, "/register") {
		s.Register(w, r)
		return
	}

	if strings.HasPrefix(r.URL.Path, "/message") {
		r.URL.Path = strings.TrimPrefix(r.URL.Path, "/message")
		s.Message(w, r)
		return
	}
	if r.Method == "POST" {
		http.Error(w, "POST needs username", http.StatusBadRequest)
		return
	}
	je := json.NewEncoder(w)
	je.Encode(s.users)
	je.Encode(s.queues)
	return
}

type agent struct {
	name, upstream, port string
}

func (ag agent) Run(addr string) error {
	_, ag.port, _ = net.SplitHostPort(addr)
	registerURL := fmt.Sprintf("%s/register/%s?port=%s", ag.upstream, ag.name, ag.port)
	var grp errgroup.Group
	grp.Go(func() error {
		register := func() error {
			req, err := http.NewRequest("PUT", registerURL, nil)
			if err != nil {
				log.Println(err)
				return err
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				log.Println(err)
				return err
			}
			log.Println("Register on " + resp.Request.URL.String() + ": " + resp.Status)
			return nil
		}

		register()
		for range time.Tick(1 * time.Minute) {
			register()
		}
		return nil
	})

	grp.Go(func() error {
		http.Handle("/", ag)
		log.Println("Listening on " + addr)
		return http.ListenAndServe(addr, nil)
	})

	return grp.Wait()
}

func (ag agent) Message(w http.ResponseWriter, r *http.Request) {
	if r.Body != nil {
		defer r.Body.Close()
	}
	req, err := http.NewRequest("POST", ag.upstream+path.Join("/message", r.URL.Path), r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	defer resp.Body.Close()
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
	return
}

func (ag agent) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	log.Println(r)
	if r != nil && r.Body != nil {
		defer r.Body.Close()
	}
	if strings.HasPrefix(r.URL.Path, "/message") {
		r.URL.Path = strings.TrimPrefix(r.URL.Path, "/message")
		ag.Message(w, r)
		return
	}

}

// vim: set fileencoding=utf-8 noet:
