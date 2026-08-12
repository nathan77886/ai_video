import { useCallback, useEffect, useMemo, useState } from 'react'

const tabs = [
  ['scripts', '剧本'],
  ['assets', '资源'],
  ['videos', '视频'],
  ['tasks', '任务'],
]

const taskStatus = {
  queued: '排队中',
  running: '处理中',
  succeeded: '已完成',
  failed: '失败',
  cancelled: '已取消',
}

async function api(path, options = {}) {
  const response = await fetch(path, options)
  if (!response.ok) {
    const body = await response.json().catch(() => ({}))
    throw new Error(body.error || `${response.status} ${response.statusText}`)
  }
  if (response.status === 204) return null
  const text = await response.text()
  return text ? JSON.parse(text) : null
}

function json(method, body) {
  return {
    method,
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  }
}

function formatBytes(size = 0) {
  if (size < 1024) return `${size} B`
  if (size < 1024 ** 2) return `${(size / 1024).toFixed(1)} KB`
  if (size < 1024 ** 3) return `${(size / 1024 ** 2).toFixed(1)} MB`
  return `${(size / 1024 ** 3).toFixed(1)} GB`
}

function formatTime(value) {
  if (!value) return '—'
  return new Intl.DateTimeFormat('zh-CN', {
    month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit',
  }).format(new Date(value))
}

export default function App() {
  const [tab, setTab] = useState('scripts')
  const [config, setConfig] = useState(null)
  const [projects, setProjects] = useState([])
  const [projectID, setProjectID] = useState(localStorage.getItem('project_id') || '')
  const [scripts, setScripts] = useState([])
  const [assets, setAssets] = useState([])
  const [videos, setVideos] = useState([])
  const [tasks, setTasks] = useState([])
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  const selectedProject = projects.find((project) => project.id === projectID)

  const run = useCallback(async (action) => {
    setBusy(true)
    setError('')
    try {
      return await action()
    } catch (err) {
      setError(err.message)
      throw err
    } finally {
      setBusy(false)
    }
  }, [])

  const loadProjects = useCallback(async () => {
    const list = await api('/api/projects')
    setProjects(list || [])
    setProjectID((current) => {
      if (list.some((project) => project.id === current)) return current
      return list[0]?.id || ''
    })
  }, [])

  const loadProjectData = useCallback(async (id) => {
    if (!id) {
      setScripts([])
      setAssets([])
      setVideos([])
      setTasks([])
      return
    }
    const query = `?project_id=${encodeURIComponent(id)}`
    const [nextScripts, nextAssets, nextVideos, nextTasks] = await Promise.all([
      api(`/api/scripts${query}`),
      api(`/api/assets${query}`),
      api(`/api/videos${query}`),
      api(`/api/tasks${query}`),
    ])
    setScripts(nextScripts || [])
    setAssets(nextAssets || [])
    setVideos(nextVideos || [])
    setTasks(nextTasks || [])
  }, [])

  useEffect(() => {
    Promise.all([api('/api/config'), loadProjects()])
      .then(([nextConfig]) => setConfig(nextConfig))
      .catch((err) => setError(err.message))
  }, [loadProjects])

  useEffect(() => {
    if (projectID) localStorage.setItem('project_id', projectID)
    else localStorage.removeItem('project_id')
    loadProjectData(projectID).catch((err) => setError(err.message))
  }, [projectID, loadProjectData])

  useEffect(() => {
    if (!projectID) return undefined
    const timer = window.setInterval(() => {
      const query = `?project_id=${encodeURIComponent(projectID)}`
      Promise.all([api(`/api/tasks${query}`), api(`/api/videos${query}`)])
        .then(([nextTasks, nextVideos]) => {
          setTasks(nextTasks || [])
          setVideos(nextVideos || [])
        })
        .catch((err) => setError(err.message))
    }, 3000)
    return () => window.clearInterval(timer)
  }, [projectID])

  async function createProject(event) {
    event.preventDefault()
    const formElement = event.currentTarget
    const form = new FormData(formElement)
    await run(async () => {
      const created = await api('/api/projects', json('POST', {
        name: form.get('name'),
        description: form.get('description'),
      }))
      formElement.reset()
      await loadProjects()
      setProjectID(created.id)
    }).catch(() => {})
  }

  async function deleteProject() {
    if (!selectedProject) return
    if (!window.confirm(`删除项目“${selectedProject.name}”及其剧本、资源、视频和任务？此操作不可恢复。`)) return
    await run(async () => {
      await api(`/api/projects/${selectedProject.id}`, { method: 'DELETE' })
      await loadProjects()
    }).catch(() => {})
  }

  return (
    <div className="shell">
      <aside className="sidebar">
        <div className="brand">
          <span className="brand-mark">AV</span>
          <div><strong>AI Video</strong><small>Production Desk</small></div>
        </div>

        <label className="field dark-field">
          <span>当前项目</span>
          <select value={projectID} onChange={(event) => setProjectID(event.target.value)}>
            <option value="">选择项目</option>
            {projects.map((project) => <option key={project.id} value={project.id}>{project.name}</option>)}
          </select>
        </label>

        <details className="project-create">
          <summary>+ 新建项目</summary>
          <form onSubmit={createProject} className="stack-form">
            <label className="field dark-field"><span>项目名</span><input name="name" required maxLength="100" /></label>
            <label className="field dark-field"><span>说明</span><textarea name="description" maxLength="1000" /></label>
            <button className="primary" disabled={busy}>创建</button>
          </form>
        </details>

        <nav aria-label="工作区">
          {tabs.map(([id, label]) => (
            <button key={id} className={tab === id ? 'nav-active' : ''} onClick={() => setTab(id)}>
              <span className={`nav-dot dot-${id}`} />{label}
              <span className="nav-count">{{ scripts, assets, videos, tasks }[id].length}</span>
            </button>
          ))}
        </nav>

        <div className="provider-state">
          <span className={config?.paid_generation_allowed && config?.minimax_configured ? 'online' : 'offline'} />
          MiniMax {config?.paid_generation_allowed && config?.minimax_configured ? '已启用' : '付费调用关闭'}
        </div>
      </aside>

      <main>
        <header className="topbar">
          <div>
            <p className="eyebrow">{tabs.find(([id]) => id === tab)?.[1]}管理</p>
            <h1>{selectedProject?.name || '先创建项目'}</h1>
            <p>{selectedProject?.description || '脚本、资源、视频、异步生成任务集中管理。'}</p>
          </div>
          {selectedProject && <button className="danger ghost" onClick={deleteProject}>删除项目</button>}
        </header>

        {error && <div className="alert" role="alert"><span>{error}</span><button onClick={() => setError('')}>关闭</button></div>}

        {!projects.length ? (
          <EmptyProject onSubmit={createProject} busy={busy} />
        ) : !selectedProject ? (
          <section className="empty"><h2>选择项目</h2><p>从左侧选择项目继续。</p></section>
        ) : (
          <>
            <Stats scripts={scripts} assets={assets} videos={videos} tasks={tasks} />
            {tab === 'scripts' && <Scripts projectID={projectID} items={scripts} busy={busy} run={run} reload={() => loadProjectData(projectID)} />}
            {tab === 'assets' && <Assets projectID={projectID} items={assets} busy={busy} run={run} reload={() => loadProjectData(projectID)} />}
            {tab === 'videos' && <Videos projectID={projectID} items={videos} busy={busy} run={run} reload={() => loadProjectData(projectID)} />}
            {tab === 'tasks' && <Tasks projectID={projectID} items={tasks} config={config} busy={busy} run={run} reload={() => loadProjectData(projectID)} />}
          </>
        )}
      </main>
    </div>
  )
}

function EmptyProject({ onSubmit, busy }) {
  return (
    <section className="empty-project panel">
      <div><p className="eyebrow">初始化工作区</p><h2>创建首个项目</h2><p>项目隔离剧本、素材、生成视频和任务记录。</p></div>
      <form onSubmit={onSubmit} className="stack-form">
        <label className="field"><span>项目名</span><input name="name" required maxLength="100" placeholder="例如：归灵司" /></label>
        <label className="field"><span>说明</span><textarea name="description" maxLength="1000" placeholder="一句话描述项目" /></label>
        <button className="primary" disabled={busy}>创建项目</button>
      </form>
    </section>
  )
}

function Stats({ scripts, assets, videos, tasks }) {
  const active = tasks.filter((task) => ['queued', 'running'].includes(task.status)).length
  return (
    <section className="stats" aria-label="项目概览">
      <article><span>剧本</span><strong>{scripts.length}</strong><small>已保存版本</small></article>
      <article><span>资源</span><strong>{assets.length}</strong><small>角色 / 场景 / 文件</small></article>
      <article><span>视频</span><strong>{videos.length}</strong><small>上传及生成结果</small></article>
      <article><span>活动任务</span><strong>{active}</strong><small>排队或处理中</small></article>
    </section>
  )
}

function Scripts({ projectID, items, busy, run, reload }) {
  const [editing, setEditing] = useState(null)
  const initial = useMemo(() => editing || { title: '', content: '' }, [editing])

  async function save(event) {
    event.preventDefault()
    const formElement = event.currentTarget
    const form = new FormData(formElement)
    await run(async () => {
      const body = { title: form.get('title'), content: form.get('content') }
      if (editing) await api(`/api/scripts/${editing.id}`, json('PATCH', body))
      else await api('/api/scripts', json('POST', { ...body, project_id: projectID }))
      setEditing(null)
      formElement.reset()
      await reload()
    }).catch(() => {})
  }

  async function remove(item) {
    if (!window.confirm(`删除剧本“${item.title}”？此操作不可恢复。`)) return
    await run(async () => { await api(`/api/scripts/${item.id}`, { method: 'DELETE' }); await reload() }).catch(() => {})
  }

  return (
    <section className="workspace two-column">
      <div className="panel">
        <div className="section-head"><div><p className="eyebrow">Script library</p><h2>剧本列表</h2></div><button className="ghost" onClick={() => setEditing(null)}>新建</button></div>
        <div className="list">
          {items.map((item) => (
            <article className={`list-item ${editing?.id === item.id ? 'selected' : ''}`} key={item.id}>
              <button className="item-main" onClick={() => setEditing(item)}>
                <strong>{item.title}</strong><span>{item.content.slice(0, 72) || '空剧本'}</span><small>{formatTime(item.updated_at)}</small>
              </button>
              <button className="icon-danger" aria-label={`删除 ${item.title}`} onClick={() => remove(item)}>×</button>
            </article>
          ))}
          {!items.length && <p className="muted">还没有剧本。</p>}
        </div>
      </div>
      <form className="panel editor" onSubmit={save} key={editing?.id || 'new'}>
        <div className="section-head"><div><p className="eyebrow">{editing ? 'Edit' : 'Create'}</p><h2>{editing ? '编辑剧本' : '新建剧本'}</h2></div>{editing && <button type="button" className="ghost" onClick={() => setEditing(null)}>取消</button>}</div>
        <label className="field"><span>标题</span><input name="title" defaultValue={initial.title} required maxLength="200" /></label>
        <label className="field grow"><span>内容</span><textarea name="content" defaultValue={initial.content} maxLength="500000" placeholder="粘贴剧本、分镜脚本或提示词草稿…" /></label>
        <button className="primary" disabled={busy}>{editing ? '保存修改' : '保存剧本'}</button>
      </form>
    </section>
  )
}

function Assets({ projectID, items, busy, run, reload }) {
  async function upload(event) {
    event.preventDefault()
    const formElement = event.currentTarget
    const form = new FormData(formElement)
    form.set('project_id', projectID)
    await run(async () => { await api('/api/assets', { method: 'POST', body: form }); formElement.reset(); await reload() }).catch(() => {})
  }
  async function remove(item) {
    if (!window.confirm(`删除资源“${item.name}”及文件？此操作不可恢复。`)) return
    await run(async () => { await api(`/api/assets/${item.id}`, { method: 'DELETE' }); await reload() }).catch(() => {})
  }
  return (
    <section className="workspace">
      <form className="panel upload-bar" onSubmit={upload}>
        <label className="field"><span>资源名</span><input name="name" required maxLength="200" placeholder="角色定妆 / 场景参考" /></label>
        <label className="field"><span>类型</span><select name="kind" defaultValue="image"><option value="character">角色</option><option value="scene">场景</option><option value="prop">道具</option><option value="costume">服装</option><option value="image">图片</option><option value="audio">音频</option><option value="document">文档</option><option value="other">其他</option></select></label>
        <label className="field file-field"><span>文件</span><input name="file" type="file" required /></label>
        <button className="primary" disabled={busy}>上传资源</button>
      </form>
      <div className="card-grid">
        {items.map((item) => (
          <article className="media-card" key={item.id}>
            <div className="asset-preview">
              {item.content_type.startsWith('image/') ? <img src={`/api/assets/${item.id}/content`} alt={item.name} /> : <span>{item.kind.toUpperCase()}</span>}
            </div>
            <div className="media-body"><span className="tag">{item.kind}</span><h3>{item.name}</h3><p>{item.filename}</p><small>{formatBytes(item.size)} · {formatTime(item.created_at)}</small></div>
            <div className="media-actions"><a className="ghost" href={`/api/assets/${item.id}/content`} target="_blank" rel="noreferrer">打开</a><button className="danger ghost" onClick={() => remove(item)}>删除</button></div>
          </article>
        ))}
        {!items.length && <section className="empty"><h2>没有资源</h2><p>上传角色、场景、道具、图片或音频。</p></section>}
      </div>
    </section>
  )
}

function Videos({ projectID, items, busy, run, reload }) {
  async function upload(event) {
    event.preventDefault()
    const formElement = event.currentTarget
    const form = new FormData(formElement)
    form.set('project_id', projectID)
    await run(async () => { await api('/api/videos', { method: 'POST', body: form }); formElement.reset(); await reload() }).catch(() => {})
  }
  async function remove(item) {
    if (!window.confirm(`删除视频“${item.title}”及文件？此操作不可恢复。`)) return
    await run(async () => { await api(`/api/videos/${item.id}`, { method: 'DELETE' }); await reload() }).catch(() => {})
  }
  return (
    <section className="workspace">
      <form className="panel upload-bar video-upload" onSubmit={upload}>
        <label className="field"><span>视频名</span><input name="name" required maxLength="200" placeholder="E001-S001-SH001 初稿" /></label>
        <label className="field file-field"><span>视频文件</span><input name="file" type="file" accept="video/*" required /></label>
        <button className="primary" disabled={busy}>上传视频</button>
      </form>
      <div className="video-grid">
        {items.map((item) => (
          <article className="video-card" key={item.id}>
            <video controls preload="metadata" src={`/api/videos/${item.id}/content`} />
            <div className="media-body"><div className="row"><span className="tag">{item.provider || 'upload'}</span>{item.model && <span className="tag muted-tag">{item.model}</span>}</div><h3>{item.title}</h3><p>{item.filename}</p><small>{formatBytes(item.size)} · {formatTime(item.created_at)}</small></div>
            <div className="media-actions"><a className="ghost" href={`/api/videos/${item.id}/content`} download={item.filename}>下载</a><button className="danger ghost" onClick={() => remove(item)}>删除</button></div>
          </article>
        ))}
        {!items.length && <section className="empty"><h2>没有视频</h2><p>上传初稿，或在任务页提交生成。</p></section>}
      </div>
    </section>
  )
}

function Tasks({ projectID, items, config, busy, run, reload }) {
  const [provider, setProvider] = useState('mock')
  const [imageMode, setImageMode] = useState(false)
  const models = imageMode
    ? ['MiniMax-Hailuo-2.3', 'MiniMax-Hailuo-2.3-Fast', 'MiniMax-Hailuo-02', 'I2V-01-Director', 'I2V-01-live', 'I2V-01']
    : ['MiniMax-Hailuo-2.3', 'MiniMax-Hailuo-02', 'T2V-01-Director', 'T2V-01']

  async function create(event) {
    event.preventDefault()
    const formElement = event.currentTarget
    const form = new FormData(formElement)
    if (provider === 'minimax' && !window.confirm('此操作会调用 MiniMax 付费视频模型。确认提交 1 个生成任务？')) return
    await run(async () => {
      await api('/api/tasks', json('POST', {
        project_id: projectID,
        provider,
        model: form.get('model'),
        prompt: form.get('prompt'),
        first_frame_image: imageMode ? form.get('first_frame_image') : '',
        duration: Number(form.get('duration')),
        resolution: form.get('resolution'),
      }))
      formElement.reset()
      await reload()
    }).catch(() => {})
  }
  async function action(id, name) {
    const warning = name === 'retry' ? '失败任务重试可能再次产生 MiniMax 费用。确认继续？' : '取消只停止本地轮询；MiniMax v1 远端生成不会停止。确认继续？'
    if (!window.confirm(warning)) return
    await run(async () => { await api(`/api/tasks/${id}/${name}`, { method: 'POST' }); await reload() }).catch(() => {})
  }
  const paidReady = config?.paid_generation_allowed && config?.minimax_configured
  return (
    <section className="workspace two-column task-layout">
      <form className="panel task-form" onSubmit={create}>
        <div className="section-head"><div><p className="eyebrow">Generation</p><h2>新建视频任务</h2></div></div>
        <div className="segmented" aria-label="生成模式"><button type="button" className={!imageMode ? 'active' : ''} onClick={() => setImageMode(false)}>文生视频</button><button type="button" className={imageMode ? 'active' : ''} onClick={() => setImageMode(true)}>图生视频</button></div>
        <label className="field"><span>执行器</span><select value={provider} onChange={(event) => setProvider(event.target.value)}><option value="mock">Mock 预演（免费）</option><option value="minimax" disabled={!paidReady}>MiniMax {paidReady ? '' : '（未启用）'}</option></select></label>
        <label className="field"><span>模型</span><select name="model" key={imageMode ? 'i2v' : 't2v'} defaultValue={models[0]}>{models.map((model) => <option key={model}>{model}</option>)}</select></label>
        {imageMode && <label className="field"><span>首帧公开 URL</span><input name="first_frame_image" type="url" required maxLength="2048" placeholder="https://…/frame.jpg" /></label>}
        <label className="field grow"><span>提示词</span><textarea name="prompt" required maxLength="2000" placeholder="人物动作、环境、镜头运动…" /></label>
        <div className="form-row"><label className="field"><span>时长</span><select name="duration" defaultValue="6"><option value="6">6 秒</option><option value="10">10 秒</option></select></label><label className="field"><span>分辨率</span><select name="resolution" defaultValue="768P"><option>512P</option><option>720P</option><option>768P</option><option>1080P</option></select></label></div>
        {provider === 'minimax' && <p className="cost-warning">将提交 1 次付费生成。失败重试最多 2 次；每次重试前仍需手工确认。</p>}
        <button className="primary" disabled={busy || (provider === 'minimax' && !paidReady)}>提交任务</button>
      </form>
      <div className="panel">
        <div className="section-head"><div><p className="eyebrow">Task center</p><h2>任务中心</h2></div><button className="ghost" onClick={reload}>刷新</button></div>
        <div className="task-list" aria-live="polite">
          {items.map((item) => (
            <article className="task-card" key={item.id}>
              <div className="row between"><div className="row"><span className={`status status-${item.status}`}>{taskStatus[item.status] || item.status}</span><span className="tag">{item.provider}</span></div><small>{formatTime(item.updated_at)}</small></div>
              <h3>{item.prompt}</h3>
              <p>{item.model} · {item.duration}s · {item.resolution}</p>
              <div className="progress"><span style={{ width: `${item.progress}%` }} /></div>
              <div className="row between"><small>{item.progress}% · 尝试 {item.attempts}/{item.max_attempts}</small><div className="row">{['queued', 'running'].includes(item.status) && <button className="danger ghost compact" onClick={() => action(item.id, 'cancel')}>取消</button>}{['failed', 'cancelled'].includes(item.status) && <button className="ghost compact" onClick={() => action(item.id, 'retry')}>重试</button>}</div></div>
              {item.error && <p className="task-error">{item.error}</p>}
              {!!item.logs?.length && <details><summary>日志 {item.logs.length}</summary><pre>{item.logs.join('\n')}</pre></details>}
            </article>
          ))}
          {!items.length && <p className="muted">还没有任务。先用 Mock 预演链路。</p>}
        </div>
      </div>
    </section>
  )
}
