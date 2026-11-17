#!/usr/bin/env python3
"""Build the upstream supplicant plus a GO-only adapter, without installing it."""
import argparse,hashlib,json,os,pathlib,shutil,subprocess,tarfile,time,urllib.request

ROOT=pathlib.Path(__file__).resolve().parent.parent
VERSION='2.12'
URL=f'https://w1.fi/releases/wpa_supplicant-{VERSION}.tar.gz'
SHA256='08e23937e16d0155e55cab2b51f51fbe10d80a1aa91c4e15442645059b737ef6'
BUILD=ROOT/'build/private-p2p'
PATCH=ROOT/'patches/wpa_supplicant-2.12-private-go.patch'
CONFIG=ROOT/'tools/p2p_supplicant.config'

def digest(path):return hashlib.sha256(path.read_bytes()).hexdigest()

def build(archive=None,jobs=4):
    if os.geteuid()==0:raise RuntimeError('请以普通用户构建，不要 sudo；只有运行网络助手需要 sudo')
    for command in ('cc','make','pkg-config','patch'):
        if not shutil.which(command):raise RuntimeError('缺少构建工具：'+command)
    subprocess.run(['pkg-config','--exists','libnl-3.0','libnl-genl-3.0','openssl'],check=True)
    BUILD.mkdir(parents=True,exist_ok=True)
    cached=BUILD/f'wpa_supplicant-{VERSION}.tar.gz'
    if archive:
        source=pathlib.Path(archive).resolve()
        if digest(source)!=SHA256:raise RuntimeError('官方源码 SHA-256 不匹配，拒绝构建')
        if source!=cached:shutil.copyfile(source,cached)
    elif not cached.is_file() or digest(cached)!=SHA256:
        print('下载官方源码（固定版本与 SHA-256）',flush=True)
        temporary=cached.with_suffix('.download')
        with urllib.request.urlopen(URL,timeout=30) as response,temporary.open('wb') as output:
            deadline=time.monotonic()+600
            while chunk:=response.read(131072):
                if time.monotonic()>deadline:raise TimeoutError('源码下载超过 10 分钟，可通过 --archive 使用本地源码包')
                output.write(chunk)
        if digest(temporary)!=SHA256:raise RuntimeError('官方源码 SHA-256 不匹配，拒绝构建')
        temporary.replace(cached)
    if digest(cached)!=SHA256:raise RuntimeError('官方源码校验失败')
    sources=BUILD/'source'
    if sources.exists():shutil.rmtree(sources)
    sources.mkdir()
    with tarfile.open(cached) as tar:tar.extractall(sources,filter='data')
    source=sources/f'wpa_supplicant-{VERSION}'
    subprocess.run(['patch','--batch','--fuzz=0','-p1','-i',str(PATCH)],cwd=source,check=True)
    shutil.copyfile(CONFIG,source/'wpa_supplicant/.config')
    print('构建项目独立副本；不执行 make install',flush=True)
    with (BUILD/'build.log').open('w') as log:
        result=subprocess.run(['make','-j'+str(jobs),'wpa_supplicant','wpa_cli'],cwd=source/'wpa_supplicant',stdout=log,stderr=subprocess.STDOUT)
    if result.returncode:
        tail='\n'.join((BUILD/'build.log').read_text(errors='replace').splitlines()[-35:])
        raise RuntimeError('构建失败：\n'+tail)
    target=BUILD/'bin';target.mkdir(exist_ok=True)
    for name in ('wpa_supplicant','wpa_cli'):
        shutil.copy2(source/'wpa_supplicant'/name,target/name)
    shutil.copyfile(source/'COPYING',target/'COPYING')
    inputs={str(p.relative_to(ROOT)):digest(p) for p in (PATCH,CONFIG,pathlib.Path(__file__).resolve())}
    manifest={'version':VERSION,'sourceUrl':URL,'sourceSha256':SHA256,'inputs':inputs,
              'binarySha256':digest(target/'wpa_supplicant'),'systemInstallationPerformed':False,
              'dbusEnabled':False,'goOnly':True}
    (target/'build.json').write_text(json.dumps(manifest,ensure_ascii=False,indent=2)+'\n')
    print(json.dumps({'binary':str(target/'wpa_supplicant'),'sourceSha256':SHA256,'systemChanged':False},ensure_ascii=False))

if __name__=='__main__':
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--archive',type=pathlib.Path,help='local official release tarball, SHA-256 checked')
    parser.add_argument('--jobs',type=int,default=4)
    args=parser.parse_args()
    if not 1<=args.jobs<=32:parser.error('--jobs 必须为 1 到 32')
    try:build(args.archive,args.jobs)
    except Exception as error:parser.exit(1,str(error)+'\n')
