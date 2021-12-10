.PHONY: build mod install

PREFIX ?= /usr/local
INSTALL ?= install
GO ?= go
prog = containerd-shim-systemd-v1

NO_NEW_NAMESPACE ?= false

build:
	$(GO) build -o bin/ .

mod:
	$(GO) mod tidy

ifeq ($(ALL), 1)
install: build
	sudo $(prog) uninstall || true
	sudo $(INSTALL) bin/* $(PREFIX)/bin
	sudo $(prog) $(ROOT_FLAGS) install --debug $(TRACEFLAGS) $(LOGMODE) --no-new-namespace=$(NO_NEW_NAMESPACE)
else
install:
	$(INSTALL) bin/* $(PREFIX)/bin
endif


test-daemon: TEST_ADDR=/run/containerd-test/containerd.sock
test-daemon: build
	sudo $(prog) uninstall || true
	sudo $(INSTALL) bin/* $(PREFIX)/bin
	sudo $(prog) --address=$(TEST_ADDR) --ttrpc-address=$(TEST_ADDR).ttrpc install --debug $(TRACEFLAGS) $(LOGMODE) --no-new-namespace=$(NO_NEW_NAMESPACE)
	if [ "$(LOGS)" = "1" ]; then sudo journalctl -u $(prog) -f --lines=0; fi