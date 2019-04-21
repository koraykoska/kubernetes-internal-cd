# base image
FROM golang:1.12.4-alpine

# add maintainer info
LABEL maintainer="Koray Koska <koray@volkn.cloud>"

# go env variables
ENV GO111MODULE on

# system dependecies
RUN apk add git

# set working directory
RUN mkdir -p $GOPATH/src/github.com/Boilertalk/volkn-kube-cd
WORKDIR $GOPATH/src/github.com/Boilertalk/volkn-kube-cd

# copy everything
COPY . $GOPATH/src/github.com/Boilertalk/volkn-kube-cd/

# download all dependencies
RUN go get -d -v ./...

# install the package
RUN go install -v ./...

# run server
CMD ["volkn-kube-cd"]
