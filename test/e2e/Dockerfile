# syntax=docker/dockerfile:1

# The e2e suite, kubectl, and the scripts that run the suite: a cluster runs the suite from this image and fetches
# nothing else. Built from the root of the repository.
ARG KUBECTL_VERSION=v1.35.4

FROM --platform=$BUILDPLATFORM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETARCH
RUN GOOS=linux GOARCH=$TARGETARCH make e2e-build
ARG KUBECTL_VERSION
RUN curl -fsSLO "https://dl.k8s.io/release/$KUBECTL_VERSION/bin/linux/$TARGETARCH/kubectl" \
	&& echo "$(curl -fsSL "https://dl.k8s.io/release/$KUBECTL_VERSION/bin/linux/$TARGETARCH/kubectl.sha256")  kubectl" | sha256sum --check \
	&& chmod +x kubectl

# The suite's own Pods run from this image too, for its shell, dd, df, stat, tail and wget.
FROM alpine:3.22
COPY --from=build /src/_output/e2e/e2e.test /src/kubectl /usr/local/bin/
# hack/e2e-run.py is here for whoever starts a run to take it from: it runs where kubectl administers the cluster.
COPY hack/e2e-pod.sh hack/e2e-run.py /usr/local/bin/
ARG KUBECTL_VERSION
# For the report of a run.
ENV KUBECTL_VERSION=$KUBECTL_VERSION
ENTRYPOINT ["/bin/sh", "/usr/local/bin/e2e-pod.sh"]
