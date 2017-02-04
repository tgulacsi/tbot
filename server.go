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
	"strings"

	"github.com/pkg/errors"
)

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
		if err := s.bot.Send(fmt.Sprintf("%d", u.LastChatID), text); err == nil {
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

func (s srv) Run() error {
	for to, texts := range s.queues {
		if u := s.usersMap[to]; u != nil && u.LastChatID != 0 {
			remains := make([]string, 0, len(texts))
			cid := fmt.Sprintf("%d", u.LastChatID)
			for _, text := range texts {
				if err := s.bot.Send(cid, text); err != nil {
					remains = append(remains, text)
				}
			}
			s.queues[to] = remains
		}
	}
	if err := s.Save(); err != nil {
		log.Println(err)
	}

	updates, err := s.bot.GetUpdatesChan(s.bot.NewUpdate(0, 60))
	if err != nil {
		log.Println(err)
	}
	var buf bytes.Buffer
	for update := range updates {
		msg := update.Message
		if msg == nil || msg.Text == "" {
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
				if err = s.bot.Send(cid, text); err != nil {
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
		URL := ag + "/execute/" + command + "?" +
			url.Values{
				"from": {msg.From.UserName},
				"args": args[1:],
			}.Encode()
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

// vim: set fileencoding=utf-8 noet:
