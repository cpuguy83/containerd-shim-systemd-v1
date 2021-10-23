.PHONY: build mod install

PREFIX ?= /usr/local
INSTALL ?= install
GO ?= go

build:
	$(GO) build -o bin/ .

mod:
	$(GO) mod tidy

install:
	$(INSTALL) bin/* $(PREFIX)/bin
