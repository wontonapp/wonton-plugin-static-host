# shellcheck shell=bash
# scripts/smoke.sh 的插件探针。本插件没有网页、没有用户和 MCP 工具；两项能力的行为由 scripts/contract.sh 对着容器检查。
# 数据放在核心的托管目录里，由用它的静态插件写，本插件自己不存数据。
export SMOKE_NOTE="数据在核心的托管目录里，由静态插件写；契约见 scripts/contract.sh"
export DATA_NA=1
