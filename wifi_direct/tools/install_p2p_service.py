#!/usr/bin/env python3
"""Install a root-owned snapshot, or stage it without privileges using --destdir."""
import argparse,getpass,hashlib,json,os,pathlib,pwd,shutil,subprocess,tempfile,sys

ROOT=pathlib.Path(__file__).resolve().parent.parent
PREFIX=pathlib.Path('/usr/local/lib/vivo-connect-p2p')
CONFIG=pathlib.Path('/etc/vivo-connect-p2p/service.json')
UNIT=pathlib.Path('/etc/systemd/system/vivo-connect-p2p.service')
INPUTS=('patches/wpa_supplicant-2.12-private-go.patch','tools/p2p_supplicant.config','tools/build_p2p_supplicant.py')
FILES=('tools/p2p_service.py','tools/p2p_persistent.py','tools/install_p2p_service.py','packaging/systemd/vivo-connect-p2p.service',
       'tools/p2p_helper.py','tools/p2p_private.py','tools/wifi_capabilities.py',*INPUTS,
       'build/private-p2p/bin/wpa_supplicant','build/private-p2p/bin/build.json','build/private-p2p/bin/COPYING')

def target(destdir,path):return destdir/path.relative_to('/')
def digest(path):return hashlib.sha256(path.read_bytes()).hexdigest()

def verify_source(source):
    for name in FILES:
        file=source/name
        if not file.is_file() or file.is_symlink():raise RuntimeError('安装源缺少普通文件：'+name)
    manifest=json.loads((source/'build/private-p2p/bin/build.json').read_text())
    if manifest.get('goOnly') is not True or manifest.get('dbusEnabled') is not False or set(manifest.get('inputs',{}))!=set(INPUTS):
        raise RuntimeError('独立 supplicant 构建记录不符合 GO 专用要求')
    if digest(source/'build/private-p2p/bin/wpa_supplicant')!=manifest.get('binarySha256'):raise RuntimeError('独立 supplicant 校验失败，请先以普通用户重新构建')
    for name in INPUTS:
        if digest(source/name)!=manifest['inputs'][name]:raise RuntimeError('构建输入发生变化，请先以普通用户重新构建')

def protect_path(path,live):
    for parent in reversed([path,*path.parents]):
        if parent.is_symlink():raise RuntimeError('安装路径不能经过符号链接：'+str(parent))
        if live and parent.exists():
            info=parent.stat()
            if info.st_uid!=0 or info.st_mode&0o022:raise RuntimeError('安装路径必须由 root 管理且不能由普通用户写入：'+str(parent))

def write_file(path,content,mode,live):
    protect_path(path,live)
    path.parent.mkdir(parents=True,exist_ok=True,mode=0o755)
    with tempfile.NamedTemporaryFile(dir=path.parent,delete=False) as stream:
        temporary=pathlib.Path(stream.name)
        try:
            stream.write(content);stream.flush();os.fchmod(stream.fileno(),mode)
            os.replace(temporary,path)
        finally:temporary.unlink(missing_ok=True)

def install(source,destdir,config,enable=True):
    live=destdir==pathlib.Path('/')
    verify_source(source)
    # Stop the installed service before replacing executable code. A manual
    # helper remains untouched and retains the exclusive group lock.
    if live and UNIT.exists():subprocess.run(['systemctl','stop','vivo-connect-p2p.service'],check=True)
    for name in FILES:
        mode=0o755 if name.endswith('/wpa_supplicant') else 0o644
        write_file(target(destdir,PREFIX/name),(source/name).read_bytes(),mode,live)
    write_file(target(destdir,CONFIG),(json.dumps(config,indent=2)+'\n').encode(),0o644,live)
    write_file(target(destdir,UNIT),(source/'packaging/systemd/vivo-connect-p2p.service').read_bytes(),0o644,live)
    if live:
        subprocess.run(['systemctl','daemon-reload'],check=True)
        if enable:subprocess.run(['systemctl','enable','--now','vivo-connect-p2p.service'],check=True)

def uninstall(destdir):
    live=destdir==pathlib.Path('/')
    if live:subprocess.run(['systemctl','disable','--now','vivo-connect-p2p.service'],check=True)
    for path in (PREFIX,CONFIG.parent,UNIT):protect_path(target(destdir,path),live)
    target(destdir,UNIT).unlink(missing_ok=True)
    target(destdir,CONFIG).unlink(missing_ok=True)
    try:target(destdir,CONFIG.parent).rmdir()
    except OSError:pass
    if target(destdir,PREFIX).exists():shutil.rmtree(target(destdir,PREFIX))
    if live:subprocess.run(['systemctl','daemon-reload'],check=True)

def refresh():
    if not CONFIG.exists():return False
    # Called by the package manager after the installed, root-owned source was
    # replaced. Retain configuration, persistent state and enable/disable choice.
    sys.path.insert(0,str(ROOT/'tools'))
    from p2p_service import load_config
    config=load_config()
    active=subprocess.run(['systemctl','is-active','--quiet','vivo-connect-p2p.service']).returncode==0
    install(ROOT,pathlib.Path('/'),config,enable=False)
    if active:subprocess.run(['systemctl','start','vivo-connect-p2p.service'],check=True)
    return True

def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action',choices=['install','uninstall','refresh'])
    parser.add_argument('--user',default=os.environ.get('SUDO_USER') or getpass.getuser())
    parser.add_argument('--interface')
    parser.add_argument('--state-dir')
    parser.add_argument('--port',type=int,default=8765)
    parser.add_argument('--destdir',type=pathlib.Path,default=pathlib.Path('/'))
    parser.add_argument('--no-enable',action='store_true')
    args=parser.parse_args();destdir=args.destdir.absolute()
    if destdir==pathlib.Path('/') and os.geteuid()!=0:parser.error('系统安装/卸载需要一次 sudo 授权；预览可用 --destdir /tmp/目录')
    if destdir==pathlib.Path('/'):
        # Only use system-managed files for privileged service operations.
        for name in FILES:protect_path(ROOT/name,True)
    if args.action=='refresh':
        if destdir!=pathlib.Path('/'):parser.error('refresh 仅更新已经安装的服务')
        if refresh():print('已有 Wi-Fi Direct 服务快照已更新，配置与启用状态保留')
        return
    if args.action=='uninstall':uninstall(destdir);print('已移除系统服务，用户账号、下载文件及后端配置保留');return
    if destdir==pathlib.Path('/'):
        if CONFIG.exists() or CONFIG.is_symlink() or UNIT.exists() or UNIT.is_symlink():
            parser.error('已有 Wi-Fi Direct 系统服务配置；请先检查现有服务')
    if not args.interface or not __import__('re').fullmatch(r'[A-Za-z0-9_][A-Za-z0-9_.:-]{0,14}',args.interface):parser.error('--interface 必须是无线网卡名')
    if not 0<args.port<65536:parser.error('--port 无效')
    user=pwd.getpwnam(args.user)
    if user.pw_uid<=0:parser.error('--user 必须是日常运行客户端的普通用户')
    if not args.state_dir:parser.error('--state-dir 必须与客户端 VIVO_STATE_DIR 一致')
    state=pathlib.Path(args.state_dir).expanduser().resolve()
    if destdir==pathlib.Path('/'):
        if not (pathlib.Path('/sys/class/net')/args.interface/'wireless').exists():parser.error('所选网卡不是无线网卡')
        for command in ('ip','nmcli','dnsmasq','systemctl'):
            if not shutil.which(command):parser.error('缺少依赖：'+command)
        info=(state/'config.json').stat()
        if info.st_uid!=user.pw_uid or info.st_mode&0o077:parser.error('状态目录 config.json 必须属于指定用户且权限为 0600；请先运行一次普通客户端')
    install(ROOT,destdir,{'uid':user.pw_uid,'user':user.pw_name,'interface':args.interface,'state_dir':str(state),'port':args.port},not args.no_enable)
    print('系统服务已安装并配置' if destdir==pathlib.Path('/') else '安装预览已写入 '+str(destdir))

if __name__=='__main__':main()
