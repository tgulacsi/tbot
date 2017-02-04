// Copyright 2017 Tamás Gulácsi. All rights reserved.

package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/pkg/errors"

	"golang.org/x/sync/errgroup"
)

type agent struct {
	name, upstream, port, baseDir string
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
	log.Printf("Message %s: %s", req.URL, resp.Status)
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

func (ag agent) execute(msg tgMsg) error {
	if msg.IsGroup() || msg.Text == "" {
		return nil
	}
	E := func(err error) error {
		log.Printf("%v", err)
		msg.Reply(fmt.Sprintf("%+v", err))
		return err
	}

	command, args := strings.TrimPrefix(msg.Text, "/"), ""
	if i := strings.IndexByte(command, ' '); i >= 0 {
		command, args = command[:i], command[i+1:]
	}
	fn := filepath.Join(ag.baseDir, command+".sh")
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
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", fn+" "+args)
	cmd.Dir = ag.baseDir
	cmd.Stdout = io.MultiWriter(&buf, os.Stdout)
	cmd.Stderr = cmd.Stdout
	cmd.Env = env
	log.Printf("calling %q", cmd.Args)
	if err := cmd.Run(); err != nil {
		return E(errors.Wrapf(err, "start %q", cmd.Args))
	}
	return msg.Reply(buf.String())
}

// vim: set fileencoding=utf-8 noet:
