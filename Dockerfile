# base image
FROM golang:1.12.4-alpine

# add maintainer info
LABEL maintainer="Koray Koska <koray@koska.at>"

# go env variables
ENV GO111MODULE on

# system dependecies
RUN apk add git

# set working directory
RUN mkdir -p $GOPATH/src/github.com/Boilertalk/kubernetes-internal-cd
WORKDIR $GOPATH/src/github.com/Boilertalk/kubernetes-internal-cd

# copy everything
COPY . $GOPATH/src/github.com/Boilertalk/kubernetes-internal-cd/

# download all dependencies
RUN go get -d -v ./...

# install the package
RUN go install -v ./...

# run server
CMD ["kubernetes-internal-cd"]
