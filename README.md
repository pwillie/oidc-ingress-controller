# oidc-ingress-controller

A Kubernetes custom controller companion to https://github.com/pwillie/oidc-ingress.  This
controller will help manage path based oidc auth ingress resources backed by the oidc-ingress
service.  Simply add an annotation to any ingress resource that requires OIDC authentication
and this controller will add all configuration necessary.

## Kubernetes Nginx Ingress OIDC sequence diagram

![OIDC Sequence Diagram](https://github.com/pwillie/oidc-ingress-controller/blob/master/images/sequence.png?raw=true "OIDC Sequence Diagram")

Created using: *https://sequencediagram.org/*

## Getting started

This project requires Go to be installed. On OS X with Homebrew you can just run `brew install go`.

Running it then should be as simple as:

```console
$ make build
$ ./bin/oidc-ingress-controller
```

### Testing

```
make test -v ./...
```
