#!/usr/bin/env bash
set -euo pipefail
: "${EVIDENCE:?Set EVIDENCE to a private output directory}"
: "${NATIVE_BUILDER_IMAGE:?Set NATIVE_BUILDER_IMAGE to the Dockerfile builder image ID}"
export IMAGE="${1:?Pass the final image ID}"
python3 - <<'PY'
import hashlib,json,os,pathlib,subprocess,time,urllib.request,uuid
E=pathlib.Path(os.environ['EVIDENCE']).resolve();E.mkdir(parents=True,exist_ok=True)
image=os.environ['IMAGE'];builder=os.environ['NATIVE_BUILDER_IMAGE']
urllib.request.install_opener(urllib.request.build_opener(urllib.request.ProxyHandler({})))
assert image.startswith('sha256:') and builder.startswith('sha256:')
def docker(*args,input=None):
 return subprocess.check_output(['docker',*args],input=input,text=True).strip()
assert docker('context','show')=='default'
assert json.loads(docker('context','inspect','default'))[0]['Endpoints']['docker']['Host']=='unix:///var/run/docker.sock'
run='forge091-file-smoke-'+uuid.uuid4().hex[:10]
pg=docker('image','inspect','postgres:17-alpine','--format','{{.Id}}')
names=[run+'-pg',run+'-auth',run+'-app']
plan={'run':run,'containers':names,'network':run,'host_ports':[],'images':{'app':image,'builder':builder,'postgres':pg},'storage':'fresh PostgreSQL and /storage tmpfs; no host mounts'}
(E/'smoke-resources.json').write_text(json.dumps(plan,indent=2))
(E/'smoke-containers-before.json').write_text(docker('ps','-a','--format','{{json .}}'))
(E/'smoke-networks-before.json').write_text(docker('network','ls','--format','{{json .}}'))
(E/'smoke-volumes-before.json').write_text(docker('volume','ls','--format','{{json .}}'))
for n in names: assert subprocess.run(['docker','inspect',n],capture_output=True).returncode!=0
assert subprocess.run(['docker','network','inspect',run],capture_output=True).returncode!=0
ids=[];network=None;result={'pass':False,'assertions':[]}
def check(label,condition):
 assert condition,label
 result['assertions'].append(label)
def ready(fn):
 for i in range(60):
  try:return fn()
  except Exception:time.sleep(1)
 raise RuntimeError('fixture readiness timed out')
try:
 network=docker('network','create','--internal',run)
 db=docker('run','-d','--name',names[0],'--network',run,'--network-alias','db','--tmpfs','/var/lib/postgresql/data','-e','POSTGRES_USER=fixture','-e','POSTGRES_PASSWORD=fixture','-e','POSTGRES_DB=fixture',pg);ids.append(db)
 ready(lambda:docker('exec',db,'pg_isready','-h','127.0.0.1','-U','fixture','-d','fixture'))
 schema='CREATE EXTENSION "uuid-ossp";\n'+pathlib.Path('db/schema/document.sql').read_text()
 docker('exec','-i',db,'psql','-v','ON_ERROR_STOP=1','-U','fixture','-d','fixture',input=schema)
 auth_source='''package main
import("net/http";"encoding/json")
func main(){p:=new(http.Protocols);p.SetHTTP1(true);p.SetUnencryptedHTTP2(true);s:=&http.Server{Addr:":6060",Protocols:p,Handler:http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){var q map[string]string;if r.URL.Path!="/internal/auth/evaluate"||json.NewDecoder(r.Body).Decode(&q)!=nil||q["actorId"]==""{http.Error(w,"bad request",400);return};w.Header().Set("Content-Type","application/json");json.NewEncoder(w).Encode(map[string]any{"allowed":true,"reason":"synthetic fixture"})})};panic(s.ListenAndServe())}
'''
 (E/'auth-fixture.go').write_text(auth_source)
 auth=docker('create','--name',names[1],'--network',run,'--network-alias','auth','--entrypoint','go',builder,'run','/tmp/auth-fixture.go');ids.append(auth)
 subprocess.run(['docker','cp',str(E/'auth-fixture.go'),auth+':/tmp/auth-fixture.go'],check=True)
 docker('start',auth)
 auth_info=json.loads(docker('inspect',auth))[0]
 auth_ip=auth_info['NetworkSettings']['Networks'][run]['IPAddress']
 def auth_ready():
  request=urllib.request.Request('http://'+auth_ip+':6060/internal/auth/evaluate',data=b'{"actorId":"fixture"}',headers={'Content-Type':'application/json'})
  with urllib.request.urlopen(request,timeout=2) as response:assert response.status==200
 ready(auth_ready)
 app=docker('run','-d','--name',names[2],'--network',run,'--tmpfs','/storage:uid=65532,gid=65532,mode=0700','-e','ALKEMIO_DATABASE_HOST=db','-e','ALKEMIO_DATABASE_PORT=5432','-e','ALKEMIO_DATABASE_USERNAME=fixture','-e','ALKEMIO_DATABASE_PASSWORD=fixture','-e','ALKEMIO_DATABASE_NAME=fixture','-e','AUTH_SERVICE_URL=http://auth:6060','-e','STORAGE_TYPE=local','-e','LOCAL_STORAGE_PATH=/storage','-e','PORT=4003',image);ids.append(app)
 (E/'smoke-created.json').write_text(json.dumps({'network':network,'containers':ids},indent=2))
 info=json.loads(docker('inspect',app))[0];ip=info['NetworkSettings']['Networks'][run]['IPAddress']
 base='http://'+ip+':4003'
 def req(path,data=None,headers=None,method=None):
  with urllib.request.urlopen(urllib.request.Request(base+path,data=data,headers=headers or {},method=method),timeout=10) as r:return r.status,dict(r.headers),r.read()
 ready(lambda:req('/health'))
 check('numeric non-root user',info['Config']['User']=='65532:65532')
 check('non-root writable storage',docker('exec',app,'sh','-ec','test "$(id -u)" = 65532; touch /storage/.writable; rm /storage/.writable')=='')
 links=docker('exec',app,'sh','-c','ldd /bin/file-service 2>&1; for module in /usr/lib/vips-modules*/*heif* /usr/lib/vips-modules*/*jxl* /usr/lib/vips-modules*/*poppler*; do test -f "$module" || exit 1; echo "$module"; ldd "$module" 2>&1; done')
 (E/'native-linkage.txt').write_text(links)
 check('native libvips linked', 'libvips.so.42' in links)
 for codec in ['heif','jxl','poppler']:check(codec+' native module linkage',codec in links)
 check('all native libraries resolved','not found' not in links and 'No such file' not in links)
 bucket=str(uuid.uuid4());actor=str(uuid.uuid4())
 def upload(name,mime,skip=False):
  payload=pathlib.Path('internal/imaging/testdata',name).read_bytes();boundary=uuid.uuid4().hex
  filepart=(f'--{boundary}\r\nContent-Disposition: form-data; name="file"; filename="{name}"\r\nContent-Type: {mime}\r\n\r\n').encode()+payload+b'\r\n'
  body=b''
  fields={'storageBucketId':bucket,'authorizationId':str(uuid.uuid4()),'displayName':name}
  if skip:fields['skipImageProcessing']='true'
  for k,v in fields.items():body+=(f'--{boundary}\r\nContent-Disposition: form-data; name="{k}"\r\n\r\n{v}\r\n').encode()
  body+=filepart
  body+=(f'--{boundary}--\r\n').encode()
  status,headers,b=req('/internal/file',body,{'Content-Type':'multipart/form-data; boundary='+boundary})
  check(name+' upload created',status==201)
  meta=json.loads(b);fileid=meta['id'];status,headers,content=req('/internal/file/'+fileid+'/content')
  check(name+' read bytes',status==200 and len(content)==meta['size'] and len(content)>0)
  check(name+' content SHA3 identity',hashlib.sha3_256(content).hexdigest()==meta['externalID'])
  if skip:check(name+' verbatim bytes',content==payload)
  else:check(name+' measured image',meta['imageWidth']>0 and meta['imageHeight']>0)
  status,headers,public=req('/rest/storage/file/'+fileid,headers={'X-Alkemio-Actor-Id':actor})
  check(name+' h2c authorized public read',status==200 and public==content)
  return meta,content
 upload('png-24bit.png','image/png',True)
 meta,content=upload('webp+alpha.webp','image/webp')
 check('WebP transformed into JPEG',content[:2]==b'\xff\xd8' and meta['mimeType']=='image/jpeg')
 upload('heic-24bit.heic','image/heic')
 result['pass']=True
finally:
 for cid in ids:
  p=subprocess.run(['docker','logs',cid],capture_output=True,text=True)
  (E/('container-'+cid[:12]+'.log')).write_text(p.stdout+p.stderr)
 cleanup=[]
 for cid in reversed(ids):cleanup.append({'id':cid,'rc':subprocess.run(['docker','rm','-f',cid],capture_output=True).returncode})
 if network:cleanup.append({'network':network,'rc':subprocess.run(['docker','network','rm',network],capture_output=True).returncode})
 result['cleanup']=cleanup
 (E/'smoke-result.json').write_text(json.dumps(result,indent=2))
PY
