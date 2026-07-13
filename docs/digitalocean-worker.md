# DigitalOcean Worker Node

This guide creates one cost-efficient DigitalOcean Droplet and registers it in the local `poolctl web` dashboard.

## Recommended Droplet

Use one Basic Regular Droplet first when Premium AMD is unavailable in the
required region:

```txt
Plan: Basic
CPU option: Regular (shared CPU)
Size: 8 GiB RAM / 4 vCPU / 160 GiB SSD / 5 TiB transfer
Estimated cost: about $48/month
Image: Ubuntu 24.04 LTS x64
Role in poolctl: worker
```

This is the current practical baseline for general backend workloads. Provider
prices, regional inventory, account limits, taxes, and credit eligibility can
change, so verify the live summary before creation. A `$200` credit does not
guarantee a fixed number of months: usage is billed while the Droplet and
eligible add-ons exist, and the credit may expire before it is exhausted. Use a
dedicated or CPU-optimized plan only after metrics show sustained CPU
contention.

## Create The Droplet

1. Open the DigitalOcean control panel.
2. Click **Create** and choose **Droplets**.
3. Pick the nearest useful region.
   - Use **Bangalore** for India-focused traffic or operator latency.
   - Use a US region if most users and upstream services are in the US.
4. Select **Ubuntu 24.04 LTS x64**.
5. Select **Basic**, then **Regular**, then **8 GiB / 4 vCPU**. If Premium AMD
   is available at a comparable price, it is also suitable.
6. Choose **SSH Key** authentication and attach your local public key.
7. Enable **Monitoring**.
8. Leave **Backups** off for the first worker. Backups add monthly cost.
9. Keep public IPv4 enabled.
10. Name the Droplet `do-worker-1`.
11. Add tags: `poolctl`, `worker`, `digitalocean`.
12. Click **Create Droplet** and copy the public IPv4 address.

## Firewall

Attach a DigitalOcean Cloud Firewall to the Droplet.

Inbound rules:

```txt
22/tcp     your current public IP only
51820/udp  Oracle/control-plane public IP only, when worker join is enabled
80/tcp     disabled unless this Droplet becomes public ingress
443/tcp    disabled unless this Droplet becomes public ingress
```

Outbound rules:

```txt
all traffic allowed
```

For the current v1 architecture, keep HTTP and HTTPS ingress on Oracle. A DigitalOcean worker should not need public 80/443 until poolctl has per-node ingress support.

## Prepare SSH User

DigitalOcean normally allows initial SSH as `root` when the key is attached. After the Droplet is ready:

```sh
ssh root@DROPLET_PUBLIC_IP
```

Create the operator user expected by this repo:

```sh
adduser ubuntu
usermod -aG sudo ubuntu
mkdir -p /home/ubuntu/.ssh
cp /root/.ssh/authorized_keys /home/ubuntu/.ssh/authorized_keys
chown -R ubuntu:ubuntu /home/ubuntu/.ssh
chmod 700 /home/ubuntu/.ssh
chmod 600 /home/ubuntu/.ssh/authorized_keys
```

Verify:

```sh
ssh ubuntu@DROPLET_PUBLIC_IP
```

The dashboard's Join operation is non-interactive, so configure passwordless
sudo narrowly for this SSH-restricted operator account:

```sh
printf 'ubuntu ALL=(ALL) NOPASSWD:ALL\n' > /etc/sudoers.d/90-poolctl
chmod 0440 /etc/sudoers.d/90-poolctl
visudo -cf /etc/sudoers.d/90-poolctl
```

Verify from the operator machine before registering the node:

```sh
ssh -o BatchMode=yes -i /absolute/path/to/worker.key ubuntu@DROPLET_PUBLIC_IP \
  'sudo -n true && echo sudo-ready'
```

## Register In poolctl Web

Start the local dashboard from the repo:

```sh
go build -o work/poolctl ./cmd/poolctl
./work/poolctl web --addr 127.0.0.1:8088
```

Open:

```txt
http://127.0.0.1:8088
```

Use **Add VPS Node**:

```txt
Name: do-worker-1
Provider: digitalocean
Public IP: DROPLET_PUBLIC_IP
SSH User: ubuntu
SSH Key: /absolute/path/to/digitalocean-worker.key
Overlay IP: keep the suggested 10.44.0.x value unless it conflicts
```

Click **Register Node**. This writes the node into `.poolctl/config.yaml` and initializes it in `.poolctl/state.yaml`.

## Join The Worker

After registration, click **Join** for `do-worker-1` in the dashboard Nodes table. The join action:

1. Generates or reuses the worker WireGuard private key on DigitalOcean.
2. Reads Oracle's WireGuard public key.
3. Adds the worker as an Oracle `wg0` peer.
4. Generates a Nomad client certificate on Oracle.
5. Copies a worker bootstrap bundle to DigitalOcean.
6. Installs Docker, Nomad, WireGuard, and the Nomad client config on the worker.
7. Starts `wg-quick@wg0` and Nomad on the worker.
8. Runs control-plane status so the new worker can be checked in Nomad.

Keep HTTP and HTTPS closed on the worker. Oracle remains the public ingress node.
