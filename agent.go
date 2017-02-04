// Copyright 2017 Tamás Gulácsi. All rights reserved.

package main

import (
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

	if strings.HasPrefix(r.URL.Path, "/execute") {
		command := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/execute"), "/", 2)[0]
		q := r.URL.Query()
		if err := ag.execute(w, q.Get("from"), command, q["args"]...); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	http.Error(w, r.URL.Path+" not found", http.StatusNotFound)
}

func (ag agent) execute(w io.Writer, from, command string, args ...string) error {
	fn := filepath.Join(ag.baseDir, command+".sh")
	fi, err := os.Stat(fn)
	if err != nil {
		return errors.Wrapf(err, fn)
	}
	if fi.Mode().Perm()&0111 == 0 {
		return errors.Wrapf(err, "permission=%s", fi.Mode().Perm())
	}
	env := append(make([]string, 0, len(os.Environ())+1), os.Environ()...)
	env = append(env, "TBOT_SENDER="+from)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", fn+" "+strings.Join(args, " "))
	cmd.Dir = ag.baseDir
	cmd.Stdout = w
	cmd.Stderr = w
	cmd.Env = env
	log.Printf("calling %q", cmd.Args)
	err = cmd.Run()
	return errors.Wrapf(err, "run %q", cmd.Args)
}

// vim: set fileencoding=utf-8 noet:
