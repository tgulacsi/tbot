// Copyright 2017 Tamás Gulácsi. All rights reserved.

package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sync"

	"go4.org/lock"

	"github.com/pkg/errors"
)

var aliases = map[string][]string{
	"lmegyesi": {"megyesilaszlo", "laszlomegyesi"},
}

type dataPath struct {
	usersFn, queuesFn string
	io.Closer
	sync.Mutex

	usersMap map[string]*User
	users    []User
	queues   map[string][]string
}

type User struct {
	Name       string
	Aliases    []string
	LastChatID int64
}

func load(dest interface{}, fn string) error {
	fh, err := os.Open(fn)
	if err != nil {
		return errors.Wrap(err, fn)
	}
	defer fh.Close()
	return json.NewDecoder(fh).Decode(dest)
}

func save(fn string, thing interface{}) error {
	fnNew := fn + ".new"
	os.Remove(fnNew)
	fh, err := os.Create(fnNew)
	if err != nil {
		return errors.Wrap(err, fnNew)
	}
	defer fh.Close()
	if err := json.NewEncoder(fh).Encode(thing); err != nil {
		return errors.Wrap(err, "encode")
	}
	if err := fh.Close(); err != nil {
		return errors.Wrap(err, fh.Name())
	}
	return os.Rename(fh.Name(), fn)
}

func newDataPath(path string) (*dataPath, error) {
	os.MkdirAll(path, 0775)
	close, err := lock.Lock(filepath.Join(path, "tbot.lock"))
	if err != nil {
		return nil, err
	}
	return &dataPath{
		usersFn:  filepath.Join(path, "users.json"),
		queuesFn: filepath.Join(path, "queues.json"),
		Closer:   close,
		users:    make([]User, 0, 32),
		queues:   make(map[string][]string, 32),
		usersMap: make(map[string]*User, 32),
	}, nil
}

func (p *dataPath) Save() error {
	p.Lock()
	err := save(p.usersFn, p.users)
	if qErr := save(p.queuesFn, p.queues); qErr != nil && err == nil {
		err = qErr
	}
	p.Unlock()
	return err
}
func (p *dataPath) Load() error {
	p.Lock()
	err := load(&p.users, p.usersFn)
	if err == nil {
		for i := 0; i < len(p.users); i++ {
			u := p.users[i]
			if u.Name == "" {
				p.users[i] = p.users[0]
				p.users = p.users[1:]
				i--
				continue
			}
			p.usersMap[u.Name] = &u
			for _, a := range u.Aliases {
				p.usersMap[a] = &u
			}
		}
	}
	if qErr := load(&p.queues, p.queuesFn); qErr != nil && err == nil {
		err = qErr
	}
	p.Unlock()
	return err
}

// vim: set fileencoding=utf-8 noet:
