
解决helm chart中的镜像, 被外部更新之后, 再进行helm upgrade会回退镜像的问题
思路: 创建一个mutating webhook, 如果发现正在进行helm更新(通过判断helm的annoatation), 而新版本更旧, 则保持原tag, 不影响镜像tag之外的其他修改. set image 方式不受影响
