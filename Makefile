all: build

TAG?=v0.1.0
REGISTRY?=ihub.helium.io:29006
FLAGS=
ENVVAR=
GOOS?=linux
ROOTPATH=`pwd` 
BUILDGOPATH=/tmp/k8splugin-build
BUILDPATH=$(BUILDGOPATH)/src/k8s-plugins/extender-controller-manager
 
.IGNORE : buildEnvClean
.IGNORE : deletedeploy 

deps:
	@go get github.com/tools/godep
	
buildEnvClean:
	@rm $(BUILDPATH) 1>/dev/null 2>/dev/null || true

buildEnv: buildEnvClean
	@mkdir -p $(BUILDGOPATH)/src/k8s-plugins/ 1>/dev/null 2>/dev/null
	@ln -s $(ROOTPATH) $(BUILDPATH)
	
build: buildEnv clean deps 
	@cd $(BUILDPATH) && GOPATH=$(BUILDGOPATH) $(ENVVAR) GOOS=$(GOOS) CGO_ENABLED=0   godep go build ./...
	@cd $(BUILDPATH) && GOPATH=$(BUILDGOPATH) $(ENVVAR) GOOS=$(GOOS) CGO_ENABLED=0   godep go build -o enndata-controller-manager pkg/main.go

docker:
ifndef REGISTRY
	ERR = $(error REGISTRY is undefined)
	$(ERR)
endif
	docker build --pull -t ${REGISTRY}/library/enndata-controller-manager:${TAG} .
	docker push ${REGISTRY}/library/enndata-controller-manager:${TAG}

deletedeploy:
	@kubectl delete -f deploy/enndata-controller-manager.yaml 1>/dev/null 2>/dev/null || true
	 
install: deletedeploy 
	@cat deploy/enndata-controller-manager.yaml | sed "s/ihub.helium.io:29006/$(REGISTRY)/g" > deploy/tmp.yaml
	kubectl create -f deploy/tmp.yaml
	@rm deploy/tmp.yaml
	 
	
uninstall: deletedeploy

release: build docker
	rm -f enndata-controller-manager

clean: buildEnvClean
	@rm -f enndata-controller-manager

format:
	test -z "$$(find . -path ./vendor -prune -type f -o -name '*.go' -exec gofmt -s -d {} + | tee /dev/stderr)" || \
	test -z "$$(find . -path ./vendor -prune -type f -o -name '*.go' -exec gofmt -s -w {} + | tee /dev/stderr)"
 