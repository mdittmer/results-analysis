# Copyright 2017 The WPT Dashboard Project. All rights reserved.
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

# Make targets in this file are intended to be run inside the Docker container
# environment.

# Make targets can be run in a host environment, but that requires ensuring
# the correct version of tools are installed and environment variables are
# set appropriately.

SHELL := /bin/bash

export GOPATH=$(shell go env GOPATH)

REPO ?= github.com/web-platform-tests/results-analysis
REPO_PATH ?= $(GOPATH)/src/$(REPO)

GO_FILES := $(wildcard $(REPO_PATH)/**/*.go)

build: deps

lint: deps
	npm install eslint babel-eslint eslint-plugin-html
	go get -u golang.org/x/lint/golint
	golint -set_exit_status $(GO_FILES)
	# Print differences between current/gofmt'd output, check empty.
	! gofmt -d $(GO_FILES) 2>&1 | read

test: deps
	npm install web-component-tester --unsafe-perm
	cd $(REPO_PATH); go test -v ./...

fmt: deps
	gofmt -w $(GO_FILES)

sys_update: sys_deps
	sudo apt-get update
	gcloud components update

deps: sys_deps go_deps py_deps

go_deps: $(GO_FILES)
	# Manual git clone + install is a workaround for #85.
	mkdir -p "$(GOPATH)/src/golang.org/x"
	cd "$(GOPATH)/src/golang.org/x" && git clone https://github.com/golang/lint
	cd "$(GOPATH)/src/golang.org/x/lint" && go get ./... && go install ./...
	cd $(REPO_PATH); go get -t ./...

py_deps: sys_deps
	if [ "$(which python) == "" ]; \
		then \
		sudo apt-get install --assume-yes --no-install-suggests python;
	fi

sys_deps:
	sudo apt-get install --assume-yes --no-install-suggests git make
	if [ "$(which gcloud)" == "" ]; \
		then \
		curl -s https://sdk.cloud.google.com > /tmp/install-gcloud.sh; \
		bash /tmp/install-gcloud.sh --disable-prompts --install-dir=/opt; \
		gcloud components install \
			app-engine-go \
			bq \
			core \
			gsutil \
			app-engine-python; \
		gcloud config set disable_usage_reporting false; \
	fi
	if [ "$(which node)" == "" ]; \
	then \
		curl -sL https://deb.nodesource.com/setup_8.x | bash; \
		sudo apt-get install -y nodejs; \
	fi
