# tsdocker

`tsdocker` is a little daemon that turns Docker containers into virtual private servers on a Tailnet; it's based on the [tsnet Tailscale blog post](https://tailscale.com/blog/tsnet-virtual-private-services/).
`tsdocker` runs a daemon with the `serve` command, and has `run`, `list`, and `stop` commands that talk to the daemon.

## Usage

Install `tsdocker` with  `go install github.com/mewil/tsdocker`.
Next, set the `TS_AUTHKEY` environment variable (using a [reusable, ephemeral token](https://tailscale.com/kb/1085/auth-keys/)). 
Then, start the daemon on a machine that has Docker running (it will use the `DOCKER_HOST` environment variable to connect).

```sh
$ TS_AUTHKEY=... tsdocker serve
2022/11/19 01:02:03 starting daemon on 0.0.0.0:8080 
```

Then in a new window, run a container and expose it on a port; the container port will be exposed and proxied to the same port on the Tailscale interface.
`tsdocker` calls the pair of container and `tsnet.Server` an instance.

```sh
$ tsdocker run --name service-0 --image strm/helloworld-http --port 80
$ tsdocker ps
NAME       IMAGE                 TAILSCALE IPS                                 PORT
service-0  strm/helloworld-http  100.79.54.159,fd7a:115c:a1e0:efe3::644f:360e  80
```

You can then access the service on the Tailscale interface at `http://100.79.54.159` or at `http://service-0`, thanks to [MagicDNS](https://tailscale.com/kb/1081/magicdns/).
Successive calls of `tsdocker run` will create new containers, each with their own Tailscale interface and IP address.

Running `tsdocker stop service-0` will stop the instance by removing the container and stopping the `tsnet.Server`. Interrupting the `tsdocker serve` command will gracefully stop all instances.