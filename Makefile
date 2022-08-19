.PHONY: build mod install

PREFIX ?= /usr/local
INSTALL ?= install
GO ?= go
prog = containerd-shim-systemd-v1

NO_NEW_NAMESPACE ?= false

TEST_IMG ?= containerd-shim-systemd-v1:local

DOCKER_BUILD ?= docker buildx build

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

build-test-image:
	$(DOCKER_BUILD) -t $(TEST_IMG) --target=test-img --load .

test-image: build-test-image
	if [ -t ]; then tty_flags="-t"; fi; \
	docker run \
		--rm \
		$${tty_flags} \
		$(EXTRA_TEST_IMAGE_FLAGS) \
		--security-opt seccomp:unconfined \
		--security-opt apparmor:unconfined \
		--security-opt label:disabled \
		--cap-add SYS_ADMIN \
		--cap-add NET_ADMIN \
		--cap-add SYS_RESOURCE \
		-e container=docker \
		--tmpfs /tmp \
		--tmpfs /run \
		--tmpfs /run/lock \
		-v /var/lib/docker \
		-v /var/lib/containerd \
		--cgroupns private \
		$(TEST_IMG)

.PHONY: bin/.test-image-cid
bin/.test-image-cid:
	if [ -f "$@" ]; then docker rm -f $$(cat $@) &> /dev/null; rm -f $(@); fi; \
	$(MAKE) -s test-image EXTRA_TEST_IMAGE_FLAGS="$(EXTRA_TEST_IMAGE_FLAGS) -d --cidfile=$(@)"

TEST_SHELL_CMD ?= bash
test-shell: bin/.test-image-cid
	@docker exec -it $$(cat $<) $(TEST_SHELL_CMD); \
	docker rm -f $$(cat $<) || true