# Sub 2 Port

- Route requests to docker containers by host name.
- Containers declare their own host name, so the config is decentralized.
- The routing table updates automatically in response to docker events.
- Ports never have to be exposed, so no more errors about ports already in use.

## Setup

Start listening on port 80:

```sh
docker network create 80
docker run -d -p 80:80 -v /var/run/docker.sock:/var/run/docker.sock:ro --network 80 deckar01/sub2port
```

## Usage

Route `test.com:80` to port 5555 in a container:

```sh
docker run -d -e SUB2PORT=test.com:5555 --network 80 your/image
```
