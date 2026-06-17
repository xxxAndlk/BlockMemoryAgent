#!/bin/bash
set -e

echo "=== 0/6 修复 DNS ==="
echo '123456' | sudo -S bash -c 'echo "nameserver 223.5.5.5" > /etc/resolv.conf && echo "nameserver 8.8.8.8" >> /etc/resolv.conf'
echo "DNS 已修复"

echo "=== 1/6 确保免密 sudo ==="
echo '123456' | sudo -S bash -c 'echo "luokun ALL=(ALL) NOPASSWD:ALL" > /etc/sudoers.d/luokun && chmod 440 /etc/sudoers.d/luokun'

echo "=== 2/6 替换 Ubuntu apt 源为阿里云 ==="
sudo cp /etc/apt/sources.list /etc/apt/sources.list.bak 2>/dev/null || true
sudo bash -c 'cat > /etc/apt/sources.list <<EOF
deb https://mirrors.aliyun.com/ubuntu/ jammy main restricted universe multiverse
deb https://mirrors.aliyun.com/ubuntu/ jammy-updates main restricted universe multiverse
deb https://mirrors.aliyun.com/ubuntu/ jammy-backports main restricted universe multiverse
deb https://mirrors.aliyun.com/ubuntu/ jammy-security main restricted universe multiverse
EOF'

echo "=== 3/6 下载 Docker GPG key (阿里云源) ==="
sudo curl -fsSL https://mirrors.aliyun.com/docker-ce/linux/ubuntu/gpg -o /tmp/docker.gpg
sudo gpg --dearmor -o /etc/apt/keyrings/docker.gpg /tmp/docker.gpg
sudo chmod a+r /etc/apt/keyrings/docker.gpg

echo "=== 4/6 添加 Docker 阿里云源 ==="
echo "deb [arch=amd64 signed-by=/etc/apt/keyrings/docker.gpg] https://mirrors.aliyun.com/docker-ce/linux/ubuntu jammy stable" | sudo tee /etc/apt/sources.list.d/docker.list > /dev/null
sudo apt-get update -y

echo "=== 5/6 安装 Docker ==="
sudo apt-get install -y docker-ce docker-ce-cli containerd.io docker-compose-plugin

echo "=== 6/6 配置 Docker 镜像加速器 ==="
sudo mkdir -p /etc/docker
sudo bash -c 'cat > /etc/docker/daemon.json <<EOF
{
  "registry-mirrors": [
    "https://docker.m.daocloud.io",
    "https://dockerproxy.com",
    "https://docker.mirrors.ustc.edu.cn",
    "https://hub-mirror.c.163.com"
  ]
}
EOF'

echo "=== 启动 Docker 服务 ==="
sudo service docker start

echo "=== 安装完成 ==="
docker --version
echo "INSTALL_DONE"
