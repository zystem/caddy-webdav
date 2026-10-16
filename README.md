# WebDAV for Caddy

This package implements a simple WebDAV handler module for Caddy.

> [!NOTE]
> This is not an official repository of the [Caddy Web Server](https://github.com/caddyserver) organization.

## Compiling

The recommended way is to use [xcaddy](https://github.com/caddyserver/xcaddy):

```sh
xcaddy build --with github.com/mholt/caddy-webdav
```

Alternatively ou can clone and build and run like this:

1. Clone `git clone https://github.com/mholt/caddy-webdav.git`
2. In the project folder, run `xcaddy` just like you would run `caddy`.
   For example: `xcaddy list-modules` and you should see the `webdav` modules.

## Syntax

```
webdav [<matcher>] {
	root <path>
	prefix <request-base-path>
}
```

Because this directive does not come standard with Caddy, you need to [put the directive in order](https://caddyserver.com/docs/caddyfile/options). The correct place is up to you, but usually putting it near the end works if no other terminal directives match the same requests. It's common to pair a webdav handler with a `file_server`, so ordering it just before is often a good choice:

```
{
	order webdav before file_server
}
```

Alternatively, you may use `route` to order it the way you want. For example:

```
localhost

root * /srv

route {
	rewrite /dav /dav/
	webdav /dav/* {
		prefix /dav
	}
	file_server
}
```

The `prefix` directive is optional but has to be used if a webdav share is used in combination with matchers or path manipulations. This is because webdav uses absolute paths in its response. There exist a similar issue when using reverse proxies, see [The "subfolder problem", OR, "why can't I reverse proxy my app into a subfolder?"](https://caddy.community/t/the-subfolder-problem-or-why-cant-i-reverse-proxy-my-app-into-a-subfolder/8575).

```
webdav /some/path/match/* {
	root /path
	prefix /some/path/match
}
```

If you want to serve WebDAV and directory listing under same path (similar behaviour as in Apache and Nginx), you may use [Request Matchers](https://caddyserver.com/docs/caddyfile/matchers) to filter out GET requests and pass those to [file_server](https://caddyserver.com/docs/caddyfile/directives/file_server).

Example with authenticated WebDAV and directory listing under the same path:

```
@get method GET HEAD

route {
    basicauth {
        username hashed_password_base64
    }
    file_server @get browse
    webdav
}
```

Or, if you want to create a public listing, but keep WebDAV behind authentication:

```
@notget not method GET HEAD

route @notget {
    basicauth {
        username hashed_password_base64
    }
    webdav
}
file_server browse
```

## Permissions

To use the WebDAV PUT command, the caddy process needs to be able to write to the storage directory. On Linux, this ordinarily means that the directory needs to be owned by the `caddy` user, or it must be world-writable (the `w` permission needs to be set for other).

Additionally, if running caddy via a systemd, it may be necessary to add the storage directory to the `ReadWriteDirectories` option of the service. For details, see [this issue](https://github.com/mholt/caddy-webdav/issues/21#issue-811534672).

## Credit

Special thanks to @hacdias for making caddy-webdav for Caddy 1, from which this work is derived: https://github.com/hacdias/caddy-webdav

## Creating the parent directories

By default a `PUT` into a collection that does not exist is refused (RFC 4918 section 9.7.1 asks for 409 Conflict). With `create_dirs` the missing parent directories are made.
The name is cleaned as an absolute path first, so a `..` in it cannot make a directory outside the root.

```
webdav [<matcher>] {
    root /data
    create_dirs
}
```

## Atomic PUT

```
webdav [<matcher>] {
    root /data
    atomic_put
    sync full        # full (the file and its directory are flushed; the default), file, none
    tmp_dir .tmp     # unfinished writes; a path into it is not found, for every method
    tmp_age 24h      # leftovers older than this are removed (at a PUT, at most once an hour)
}
```

A `PUT` is written to a file in `tmp_dir` (in the root, so on the same file system), flushed, and renamed to the name of the object; then the directory is flushed. A crash or a
client that goes away leaves a file in `tmp_dir`, never a part of an object under its name, and an object that was there stays whole. Without `create_dirs` the parent collection
must exist (409 if not). A `PUT` with `Content-Range` is refused (501); WebDAV locks are not consulted by an atomic `PUT`.

`If-None-Match: *` makes an atomic `PUT` create-only: 412 Precondition Failed if the object exists. The check and the creation are one operation (a hard link), so of many
writers of one name exactly one wins.

A client that sends the SHA-256 of its body in `X-Content-SHA256` (64 hexadecimal digits) has it checked while the body is written; on a mismatch the answer is 422 and nothing
is placed. The answer of an atomic `PUT` carries the SHA-256 of what was written in the same header.

`GET` and `HEAD` with the query `sha256` answer the SHA-256 of an object in `X-Content-SHA256` (and in the body of a `GET`) without sending the object. The module has to see
such a request, so give it its own matcher:

```
@hash query sha256=*
webdav @hash {
    root /data
    atomic_put
}
```

The hash is kept in the extended attribute `user.cinim.sha256` together with the size and the modification time of the file it was taken from, and is taken again when they
differ (or when the file system has no extended attributes).

