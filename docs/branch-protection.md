# Branch protection reads

A session can read classic branch protection through the broker:

```sh
beekeeper sandbox protection giantswarm/example main
beekeeper sandbox protection giantswarm/example release/v1
```

The result is the GitHub protection JSON. The broker uses its own authenticated
`gh` login, outside the agent shell's executable paths and ignoring session token
environment variables. The credential never enters the session. That login must
have access to the repository and permission to read its administration settings.
This does not grant additional permissions to the session's GitHub App token.

The broker accepts exactly a repository and branch, limits repositories to
`github.allowedOrgs`, and sends a fixed GET. It accepts no method, URL, body,
or write arguments. Protection writes are unavailable through this operation.
The broker does not follow redirects or return upstream error bodies. HTTP 403
and 404 are errors, never evidence that protection is disabled. Repository
rulesets are separate and are not included in this classic protection response.

Start the updated `beekeeper sandbox broker` service before using the command.
A missing authenticated login or insufficient access fails explicitly; the
command never changes a login, App permission, or protection setting.
