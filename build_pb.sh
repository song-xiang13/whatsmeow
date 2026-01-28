#!/usr/bin/env bash
set -e

# 此脚本支持在win、linux、mac环境下执行

# shellcheck disable=SC2188
<<EOF
使用示例：
sh build_pb.sh <proto_dir>  # 生成指定目录的pb
sh build_pb.sh               # 生成所有pb
EOF

PROTO_DIR=$1
_protoc_path=""

if [[ $(uname) == "Linux" ]]; then
  echo "running on Linux"
  _protoc_path='./tool/linux/protoc_v24'
  chmod +x $_protoc_path/*
elif [[ $(uname) == "Darwin" ]]; then
  echo "running on macOS"
  _protoc_path='./tool/mac/protoc_v24'
  chmod +x $_protoc_path/*
elif [[ $(uname) == *MINGW* ]]; then
  echo "running on Windows"
  _protoc_path='./tool/win/protoc_v24'
else
  echo "unknown OS"
  exit 1
fi

PATH=$PATH:$_protoc_path

case $PROTO_DIR in
clear)
  echo "清理所有 .pb.go 文件..."
  find ./proto -name "*.pb.go" -delete
  exit
  ;;
esac

# 定义 protoc 命令
__protoc_gen='protoc -I ./proto/ \
          --go_out=./proto/. \
          --go_opt paths=source_relative'

if [[ -n $PROTO_DIR ]]; then
  echo "生成 proto/$PROTO_DIR/*.proto..."
  eval "$__protoc_gen proto/$PROTO_DIR/*.proto"
else
  echo "生成所有 proto 文件..."
  for proto_file in ./proto/*/*.proto; do
    if [ -f "$proto_file" ]; then
      echo "处理 $proto_file..."
      eval "$__protoc_gen $proto_file"
    fi
  done
fi

echo "完成！"
