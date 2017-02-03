// Copyright 2017 Tamás Gulácsi. All rights reserved.

package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/errors"
	"golang.org/x/net/context"

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
	flagAddr := flag.String("http", ":"+strconv.Itoa(Port), "HTTP address to listen on")
	flagData := flag.String("data", BotBaseDir, "data path")
	flagVerbose := flag.Bool("v", false, "verbose logging")
	flag.Parse()

	getBot := func() tgBot {
		if token == "" {
			log.Fatal("You have to set environment variable TELEGRAM_TOKEN first!")
		}
		ba, err := tgbotapi.NewBotAPI(token)
		if err != nil {
			log.Fatalf("Error creating bot: %v", err)
		}
		ba.Debug = *flagVerbose
		bot := tgBot{BotAPI: ba}
		log.Printf("Bot started with %q.", bot.Self.UserName)
		return bot
	}

	if flag.NArg() > 1 { // just send this message
		to := flag.Arg(0)
		text := strings.Join(flag.Args()[1:], " ")

		req, err := http.NewRequest(
			"POST",
			fmt.Sprintf("http://%s/%s", *flagAddr, to),
			strings.NewReader(text),
		)
		if err != nil {
			log.Fatal(err)
		}
		log.Printf("Calling %s", req.URL)
		resp, err := http.DefaultClient.Do(req)
		if resp != nil && resp.Body != nil {
			defer resp.Body.Close()
		}
		if err == nil {
			log.Println(resp.Status)
			io.Copy(os.Stdout, resp.Body)
			return nil
		}
		log.Println(err)

		log.Println("Opening " + *flagData)
		data, err := newDataPath(*flagData)
		if err != nil {
			log.Fatal(err)
		}
		if err := data.Load(); err != nil {
			log.Println(err)
		}
		if u := data.usersMap[to]; u != nil && u.LastChatID != 0 {
			log.Println("Sending to " + to)
			bot := getBot()
			_, err = bot.Send(tgbotapi.NewMessageToChannel(fmt.Sprintf("%d", u.LastChatID), text))
			if err == nil {
				return nil
			}
		}
		log.Println("Saving queue")
		data.queues[to] = append(data.queues[to], text)
		if saveErr := data.Save(); saveErr != nil && err == nil {
			err = saveErr
		}
		return err
	}

	log.Println("Opening " + *flagData)
	data, err := newDataPath(*flagData)
	if err != nil {
		log.Println(err)
	}
	bot := getBot()
	http.Handle("/", srv{dataPath: data, bot: bot})
	go func() {
		log.Println("Listening on " + *flagAddr)
		log.Fatal(http.ListenAndServe(*flagAddr, nil))
	}()

	if err := data.Load(); err != nil {
		log.Println(err)
	}
	for to, texts := range data.queues {
		if u := data.usersMap[to]; u != nil && u.LastChatID != 0 {
			remains := make([]string, 0, len(texts))
			cid := fmt.Sprintf("%d", u.LastChatID)
			for _, text := range texts {
				if _, err = bot.Send(tgbotapi.NewMessageToChannel(cid, text)); err != nil {
					remains = append(remains, text)
				}
			}
			data.queues[to] = remains
		}
	}
	if err := data.Save(); err != nil {
		log.Println(err)
	}

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates, err := bot.GetUpdatesChan(u)
	if err != nil {
		log.Println(err)
	}
	for update := range updates {
		msg := update.Message
		if msg == nil {
			continue
		}
		log.Printf("[%s] %s", msg.From.UserName, msg.Text)
		uname := msg.From.UserName
		u := data.usersMap[uname]
		if u == nil {
			u = &User{Name: msg.From.UserName, Aliases: aliases[msg.From.UserName]}
			data.users = append(data.users, *u)
			i := len(data.users) - 1
			data.usersMap[uname] = &data.users[i]
		}
		if u != nil && u.LastChatID != msg.Chat.ID {
			data.usersMap[uname].LastChatID = msg.Chat.ID
			var remains []string
			cid := fmt.Sprintf("%d", msg.Chat.ID)
			for _, text := range data.queues[uname] {
				if _, err = bot.Send(tgbotapi.NewMessageToChannel(cid, text)); err != nil {
					remains = append(remains, text)
				}
			}
			data.queues[uname] = remains
			if err := data.Save(); err != nil {
				log.Println(err)
			}
		}

		command := msg.Text
		if i := strings.IndexByte(command, ' '); i >= 0 {
			command = command[:i]
		}
		switch command {
		case "/help":
			bot.Reply(msg,
				`/help Show this message.
/doku	Mi változott, mit kellene dokumentálni?
/oerr	Oracle hibakód kereső
/forward Forward last message.

or you can send message to me, I will reply it with some debug message.`)
		//case "/forward":
		//bot.ForwardMessage(msg.Chat, msg.Chat, msg.ID)
		default:
			execute(bot, msg)
		}
	}
	return nil
}

func execute(bot tgBot, msg *tgbotapi.Message) error {
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
	bot tgBot
}

func (s srv) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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

// vim: set fileencoding=utf-8 noet:
