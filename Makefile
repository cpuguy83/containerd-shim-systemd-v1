.PHONY: build mod install

PREFIX ?= /usr/local
INSTALL ?= install
GO ?= go
prog = containerd-shim-systemd-v1

build:
	$(GO) build -o bin/ .

mod:
	$(GO) mod tidy

ifeq ($(ALL), 1)
install: build
	sudo $(prog) uninstall || true
	sudo $(INSTALL) bin/* $(PREFIX)/bin
	sudo $(prog) install --debug
else
install:
	$(INSTALL) bin/* $(PREFIX)/bin
endif