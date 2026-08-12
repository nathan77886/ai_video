import { useCallback, useEffect, useMemo, useState } from 'react'

const tabs = [
  ['shots', '分镜审核'],
  ['assets', '素材库'],
  ['videos', '试片'],
  ['tasks', '任务'],
]

const reviewLabels = {
  pending: '待审核',
  approved: '已通过',
  changes_requested: '需修改',
  rejected: '已驳回',
}

const taskLabels = {
  queued: '排队中',
  running: '处理中',
  succeeded: '已完成',
  failed: '失败',
  cancelled: '已取消',
}

const taskStatusOrder = { running: 0, queued: 1, failed: 2, cancelled: 3 }

const kindLabels = {
  character: '角色', scene: '场景', prop: '道具', costume: '服装',
  image: '图片', audio: '音频', document: '文档', other: '其他',
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
  return { method, headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) }
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

function isTextAsset(item) {
  return item.content_type?.startsWith('text/') || item.content_type === 'application/json' || /\.(jsonl?|md|txt|py|go|ya?ml)$/i.test(item.filename)
}

function ImageLightbox({ viewer, onClose, onSelect }) {
  const { images, index, title } = viewer
  const current = images[index]

  useEffect(() => {
    const previousOverflow = document.body.style.overflow
    document.body.style.overflow = 'hidden'
    const handleKeyDown = (event) => {
      if (event.key === 'Escape') onClose()
      if (images.length > 1 && event.key === 'ArrowLeft') onSelect((index - 1 + images.length) % images.length)
      if (images.length > 1 && event.key === 'ArrowRight') onSelect((index + 1) % images.length)
    }
    window.addEventListener('keydown', handleKeyDown)
    return () => {
      document.body.style.overflow = previousOverflow
      window.removeEventListener('keydown', handleKeyDown)
    }
  }, [images.length, index, onClose, onSelect])

  return <div className="image-lightbox-backdrop" role="presentation" onMouseDown={onClose}>
    <section className="image-lightbox" role="dialog" aria-modal="true" aria-labelledby="image-lightbox-title" onMouseDown={(event) => event.stopPropagation()}>
      <header className="image-lightbox-head">
        <div><p id="image-lightbox-title">{title}</p><span>{current.label} · {index + 1}/{images.length}</span></div>
        <div className="row"><a href={current.src} target="_blank" rel="noreferrer">查看原图</a><button type="button" aria-label="关闭大图预览" onClick={onClose}>×</button></div>
      </header>
      <div className="image-lightbox-stage">
        {images.length > 1 && <button type="button" className="image-lightbox-nav previous" aria-label="上一张" onClick={() => onSelect((index - 1 + images.length) % images.length)}>‹</button>}
        <img src={current.src} alt={current.alt || current.label} draggable="false" />
        {images.length > 1 && <button type="button" className="image-lightbox-nav next" aria-label="下一张" onClick={() => onSelect((index + 1) % images.length)}>›</button>}
      </div>
      {images.length > 1 && <div className="image-lightbox-thumbnails" aria-label="图片列表">{images.map((image, imageIndex) => <button type="button" className={imageIndex === index ? 'selected' : ''} aria-label={`查看${image.label}`} aria-current={imageIndex === index ? 'true' : undefined} key={image.id} onClick={() => onSelect(imageIndex)}><img src={image.src} alt="" /><span>{image.label}</span></button>)}</div>}
    </section>
  </div>
}

export default function App() {
  const [tab, setTab] = useState('shots')
  const [config, setConfig] = useState(null)
  const [projects, setProjects] = useState([])
  const [projectID, setProjectID] = useState(localStorage.getItem('project_id') || '')
  const [inputVersion, setInputVersion] = useState(localStorage.getItem('input_version') || '')
  const [shots, setShots] = useState([])
  const [assets, setAssets] = useState([])
  const [videos, setVideos] = useState([])
  const [tasks, setTasks] = useState([])
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  const selectedProject = projects.find((project) => project.id === projectID)
  const versions = useMemo(() => [...new Set(shots.map((shot) => shot.input_version).filter(Boolean))].sort().reverse(), [shots])
  const selectedVersion = versions.includes(inputVersion) ? inputVersion : versions[0] || ''
  const visibleShots = useMemo(() => shots.filter((shot) => !selectedVersion || shot.input_version === selectedVersion), [shots, selectedVersion])
  const visibleShotIDs = useMemo(() => new Set(visibleShots.map((shot) => shot.id)), [visibleShots])
  const visibleAssetIDs = useMemo(() => new Set(assets.map((asset) => asset.id)), [assets])
  const visibleTasks = useMemo(() => tasks.filter((task) => visibleShotIDs.has(task.shot_id) || visibleAssetIDs.has(task.asset_id)), [tasks, visibleShotIDs, visibleAssetIDs])
  const actionableTasks = useMemo(() => visibleTasks.filter((task) => task.status !== 'succeeded').sort((a, b) => (taskStatusOrder[a.status] ?? 99) - (taskStatusOrder[b.status] ?? 99) || new Date(b.updated_at) - new Date(a.updated_at)), [visibleTasks])
  const visibleVideos = useMemo(() => videos.filter((video) => visibleShotIDs.has(video.shot_id)), [videos, visibleShotIDs])
  const visibleAssets = useMemo(() => assets.filter((asset) => !asset.links?.length || asset.links.some((link) => link.target_type !== 'shot' || visibleShotIDs.has(link.target_id))), [assets, visibleShotIDs])

  const run = useCallback(async (action) => {
    setBusy(true)
    setError('')
    try { return await action() } catch (err) { setError(err.message); throw err } finally { setBusy(false) }
  }, [])

  const loadProjects = useCallback(async () => {
    const list = await api('/api/projects')
    setProjects(list || [])
    setProjectID((current) => list.some((project) => project.id === current) ? current : list[0]?.id || '')
  }, [])

  const loadProjectData = useCallback(async (id) => {
    if (!id) {
      setShots([]); setAssets([]); setVideos([]); setTasks([])
      return
    }
    const query = `?project_id=${encodeURIComponent(id)}`
    const [nextShots, nextAssets, nextVideos, nextTasks] = await Promise.all([
      api(`/api/shots${query}`), api(`/api/assets${query}`), api(`/api/videos${query}`), api(`/api/tasks${query}`),
    ])
    setShots(nextShots || [])
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
    if (selectedVersion) localStorage.setItem('input_version', selectedVersion)
    else localStorage.removeItem('input_version')
  }, [selectedVersion])

  useEffect(() => {
    if (!projectID) return undefined
    const timer = window.setInterval(() => loadProjectData(projectID).catch((err) => setError(err.message)), 4000)
    return () => window.clearInterval(timer)
  }, [projectID, loadProjectData])

  async function createProject(event) {
    event.preventDefault()
    const formElement = event.currentTarget
    const form = new FormData(formElement)
    await run(async () => {
      const created = await api('/api/projects', json('POST', { name: form.get('name'), description: form.get('description') }))
      formElement.reset()
      await loadProjects()
      setProjectID(created.id)
    }).catch(() => {})
  }

  async function deleteProject() {
    if (!selectedProject || !window.confirm(`删除项目“${selectedProject.name}”及全部分镜、素材、试片和任务？此操作不可恢复。`)) return
    await run(async () => { await api(`/api/projects/${selectedProject.id}`, { method: 'DELETE' }); await loadProjects() }).catch(() => {})
  }

  const counts = { shots: visibleShots.length, assets: visibleAssets.length, videos: visibleVideos.length, tasks: actionableTasks.length }
  const reload = () => loadProjectData(projectID)

  return (
    <div className="shell">
      <aside className="sidebar">
        <div className="brand"><span className="brand-mark">30K</span><div><strong>镜头生产台</strong><small>Warhammer × Azeroth</small></div></div>
        <label className="field dark-field"><span>当前项目</span><select value={projectID} onChange={(event) => setProjectID(event.target.value)}><option value="">选择项目</option>{projects.map((project) => <option key={project.id} value={project.id}>{project.name}</option>)}</select></label>
        {versions.length > 0 && <label className="field dark-field"><span>资源版本</span><select value={selectedVersion} onChange={(event) => setInputVersion(event.target.value)}>{versions.map((version) => <option key={version} value={version}>{version}</option>)}</select></label>}
        <details className="project-create"><summary>+ 新建项目</summary><form onSubmit={createProject} className="stack-form"><label className="field dark-field"><span>项目名</span><input name="name" required maxLength="100" /></label><label className="field dark-field"><span>说明</span><textarea name="description" maxLength="1000" /></label><button className="primary" disabled={busy}>创建</button></form></details>
        <nav aria-label="工作区">{tabs.map(([id, label]) => <button key={id} className={tab === id ? 'nav-active' : ''} onClick={() => setTab(id)}><span className={`nav-dot dot-${id}`} />{label}<span className="nav-count">{counts[id]}</span></button>)}</nav>
        <div className="provider-state"><span className={config?.openai_image_configured ? 'online' : 'offline'} />GPT Image 2 {config?.openai_image_configured ? '图片队列已启用' : '未配置'}</div>
      </aside>

      <main>
        <header className="topbar"><div><p className="eyebrow">{tabs.find(([id]) => id === tab)?.[1]}</p><h1>{selectedProject?.name || '先创建项目'}</h1><p>{selectedProject?.description || '先审分镜和提示词，再决定是否生成。'}</p></div>{selectedProject && <button className="danger ghost" onClick={deleteProject}>删除项目</button>}</header>
        {error && <div className="alert" role="alert"><span>{error}</span><button onClick={() => setError('')}>关闭</button></div>}
        {!projects.length ? <EmptyProject onSubmit={createProject} busy={busy} /> : !selectedProject ? <section className="empty"><h2>选择项目</h2></section> : <>
          <Overview shots={visibleShots} tasks={visibleTasks} />
          {tab === 'shots' && <Shots items={visibleShots} assets={visibleAssets} videos={visibleVideos} tasks={visibleTasks} config={config} busy={busy} run={run} reload={reload} />}
          {tab === 'assets' && <Assets projectID={projectID} items={visibleAssets} shots={visibleShots} videos={visibleVideos} tasks={visibleTasks} config={config} busy={busy} run={run} reload={reload} />}
          {tab === 'videos' && <Videos projectID={projectID} items={visibleVideos} busy={busy} run={run} reload={reload} />}
          {tab === 'tasks' && <Tasks items={actionableTasks} busy={busy} run={run} reload={reload} />}
        </>}
      </main>
    </div>
  )
}

function EmptyProject({ onSubmit, busy }) {
  return <section className="empty-project panel"><div><p className="eyebrow">初始化工作区</p><h2>创建首个项目</h2><p>项目隔离分镜、素材、试片和任务。</p></div><form onSubmit={onSubmit} className="stack-form"><label className="field"><span>项目名</span><input name="name" required maxLength="100" /></label><label className="field"><span>说明</span><textarea name="description" maxLength="1000" /></label><button className="primary" disabled={busy}>创建项目</button></form></section>
}

function Overview({ shots, tasks }) {
  const approved = shots.filter((shot) => shot.review_status === 'approved').length
  const pending = shots.filter((shot) => shot.review_status === 'pending').length
  const ready = shots.filter((shot) => shot.review_status === 'approved' && shot.generation_route === 'video_api' && !shot.requires_editorial_split).length
  const active = tasks.filter((task) => ['queued', 'running'].includes(task.status)).length
  return <section className="stats overview-stats" aria-label="制作概览"><article><span>全部镜头</span><strong>{shots.length}</strong><small>{new Set(shots.map((shot) => shot.episode_id)).size} 集</small></article><article><span>待审核</span><strong>{pending}</strong><small>等待人工判断</small></article><article><span>已通过</span><strong>{approved}</strong><small>包含后期路线</small></article><article><span>可生成</span><strong>{ready}</strong><small>视频 API 且无需拆镜</small></article><article><span>活动任务</span><strong>{active}</strong><small>排队或处理中</small></article></section>
}

function Shots({ items, assets, videos, tasks, config, busy, run, reload }) {
  const [episode, setEpisode] = useState('')
  const [status, setStatus] = useState('')
  const [route, setRoute] = useState('')
  const [query, setQuery] = useState('')
  const [selectedID, setSelectedID] = useState('')
  const [note, setNote] = useState('')
  const [useFrameImages, setUseFrameImages] = useState(false)
  const [characterPromptCount, setCharacterPromptCount] = useState(0)
  const [referenceImageIDs, setReferenceImageIDs] = useState([])
  const [linking, setLinking] = useState(false)
  const [imageViewer, setImageViewer] = useState(null)

  const episodes = useMemo(() => [...new Set(items.map((shot) => shot.episode_id))].sort(), [items])
  const filtered = useMemo(() => items.filter((shot) => {
    const haystack = `${shot.id} ${shot.chapter_title} ${shot.visual} ${shot.prompt}`.toLowerCase()
    return (!episode || shot.episode_id === episode) && (!status || shot.review_status === status) && (!route || shot.generation_route === route) && (!query || haystack.includes(query.toLowerCase()))
  }), [items, episode, status, route, query])
  const selected = filtered.find((shot) => shot.id === selectedID) || filtered[0]

  useEffect(() => { if (selected && selected.id !== selectedID) setSelectedID(selected.id) }, [selected, selectedID])
  useEffect(() => { setNote(selected?.review_note || '') }, [selected?.id, selected?.review_note])

  const shotVideo = videos.find((video) => video.id === selected?.video_id || video.shot_id === selected?.id)
  const shotVideoTask = tasks.find((task) => task.id === selected?.task_id) || tasks.find((task) => task.shot_id === selected?.id && task.kind === 'video_generation')
  const hasActiveImageTask = tasks.some((task) => task.shot_id === selected?.id && task.kind === 'image_generation' && ['queued', 'running'].includes(task.status))
  const linkedAssets = selected?.asset_links?.map((link) => ({ link, asset: assets.find((asset) => asset.id === link.asset_id) })).filter((item) => item.asset) || []
  const generatedImages = linkedAssets.filter(({ link, asset }) => link.note?.startsWith('GPT Image 2 ') && asset.content_type.startsWith('image/'))
  const shotImages = generatedImages.map(({ link, asset }) => ({ id: asset.id, src: `/api/assets/${asset.id}/content`, label: link.note.replace('GPT Image 2 ', ''), alt: `${selected?.id} ${link.note}` }))
  const referenceImages = assets.filter((asset) => asset.content_type?.startsWith('image/'))
  const canGenerate = selected?.review_status === 'approved' && selected?.generation_route === 'video_api' && !selected?.requires_editorial_split

  async function review(nextStatus) {
    await run(async () => { await api(`/api/shots/${selected.id}/review`, json('PATCH', { status: nextStatus, note })); await reload() }).catch(() => {})
  }

  async function generate(provider) {
    if (provider === 'minimax') {
      const frameMode = useFrameImages ? '首尾帧：作为图像输入' : '首尾帧：不送入请求'
      const characterMode = characterPromptCount ? `角色模型：展开 ${characterPromptCount} 个到提示词` : '角色模型：不展开到提示词'
      const referenceMode = referenceImageIDs.length ? `参考图：${referenceImageIDs.length} 张作为 reference_image` : '参考图：不送入请求'
      const warning = `此操作会调用付费 MiniMax 视频模型。\n\n待生成镜头：1\n预计付费调用：1 次\n输入版本：${selected.input_version || '03_生成提示词/v001'}\n${frameMode}\n${referenceMode}\n${characterMode}\n目标目录：/data/media/${selected.project_id}/videos/<task_id>/\n\n确认继续？`
      if (!window.confirm(warning)) return
    }
    await run(async () => { await api(`/api/shots/${selected.id}/generate`, json('POST', { provider, resolution: '768P', use_frame_images: useFrameImages, character_prompt_count: characterPromptCount, reference_image_ids: referenceImageIDs })); await reload() }).catch(() => {})
  }

  async function generateImages() {
    await run(async () => { await api(`/api/shots/${selected.id}/images/generate`, { method: 'POST' }); await reload() }).catch(() => {})
  }

  async function linkAsset(event) {
    event.preventDefault()
    const form = new FormData(event.currentTarget)
    await run(async () => { await api(`/api/shots/${selected.id}/assets`, json('POST', { asset_id: form.get('asset_id'), note: form.get('note') })); setLinking(false); await reload() }).catch(() => {})
  }

  async function unlink(linkID) {
    await run(async () => { await api(`/api/shots/${selected.id}/assets/${linkID}`, { method: 'DELETE' }); await reload() }).catch(() => {})
  }

  return <section className="workspace shot-workspace">
    <div className="panel shot-index">
      <div className="section-head"><div><p className="eyebrow">Storyboard queue</p><h2>镜头队列</h2></div><strong>{filtered.length}</strong></div>
      <div className="shot-filters"><select value={episode} onChange={(event) => setEpisode(event.target.value)}><option value="">全部集</option>{episodes.map((id) => <option key={id}>{id}</option>)}</select><select value={status} onChange={(event) => setStatus(event.target.value)}><option value="">全部状态</option>{Object.entries(reviewLabels).map(([value, label]) => <option value={value} key={value}>{label}</option>)}</select><select value={route} onChange={(event) => setRoute(event.target.value)}><option value="">全部路线</option><option value="video_api">视频 API</option><option value="post_production">后期制作</option></select><input value={query} onChange={(event) => setQuery(event.target.value)} placeholder="搜镜头、画面、提示词" /></div>
      <div className="shot-list">{filtered.map((shot) => <button key={shot.id} className={`shot-list-item ${selected?.id === shot.id ? 'selected' : ''}`} onClick={() => setSelectedID(shot.id)}><div className="row between"><strong>{shot.id}</strong><span className={`review review-${shot.review_status}`}>{reviewLabels[shot.review_status]}</span></div><p>{shot.visual}</p><small>{shot.duration_sec}s · {shot.framing} · {shot.generation_route === 'video_api' ? '视频 API' : '后期制作'}</small></button>)}{!filtered.length && <p className="muted">没有匹配镜头。</p>}</div>
    </div>

    {selected ? <article className="panel shot-detail">
      <div className="section-head shot-title"><div><div className="row wrap"><span className={`review review-${selected.review_status}`}>{reviewLabels[selected.review_status]}</span><span className="tag">{selected.episode_id}</span><span className="tag muted-tag">第 {selected.chapter} 章</span></div><h2>{selected.id}</h2><p>{selected.chapter_title}</p></div><div className="route-stack"><span className={`route route-${selected.generation_route}`}>{selected.generation_route === 'video_api' ? '视频 API' : '后期制作'}</span>{selected.requires_editorial_split && <span className="route split">需要拆镜</span>}</div></div>
      <section className="shot-section"><p className="eyebrow">画面设计</p><h3>{selected.visual}</h3><div className="metadata"><span><b>景别</b>{selected.framing || '—'}</span><span><b>运镜</b>{selected.camera || '—'}</span><span><b>音频</b>{selected.audio || '—'}</span><span><b>规格</b>{selected.duration_sec}s · {selected.aspect_ratio}</span><span><b>模式</b>{selected.source_mode || '—'}</span><span><b>模型</b>{selected.target_model}</span></div></section>
      <section className="shot-section prompt-block"><div className="row between"><p className="eyebrow">MiniMax H3 提示词</p><small>{selected.prompt.length} 字</small></div><p>{selected.prompt}</p><details><summary>负面提示词</summary><p>{selected.negative_prompt || '无'}</p></details></section>
      {(selected.generation_route === 'post_production' || selected.requires_editorial_split) && <div className="gate-warning">{selected.generation_route === 'post_production' ? '此镜头走后期制作路线，不直接调用视频 API。' : '此镜头需先拆成单一动作镜头，再进入生成。'}</div>}
      <section className="shot-section"><div className="section-head compact-head"><div><p className="eyebrow">关联素材</p><h3>{linkedAssets.length} 项</h3></div><button className="ghost compact" onClick={() => setLinking(true)}>+ 关联素材</button></div><div className="linked-assets">{linkedAssets.map(({ link, asset }) => <span className="link-chip" key={link.id}>{kindLabels[asset.kind]} · {asset.name}{link.note && ` · ${link.note}`}<button aria-label="移除关联" onClick={() => unlink(link.id)}>×</button></span>)}{!linkedAssets.length && <span className="muted">尚未关联角色、场景或参考文件。</span>}</div></section>
      {shotImages.length > 0 && <section className="shot-section"><p className="eyebrow">GPT Image 2 镜头图片</p><div className="shot-images">{shotImages.map((image, index) => <button type="button" key={image.id} onClick={() => setImageViewer({ title: selected.id, images: shotImages, index })}><img src={image.src} alt={image.alt} /><span>{image.label}</span></button>)}</div></section>}
      {(shotVideo || shotVideoTask) && <section className="shot-section result-grid">{shotVideo && <div><p className="eyebrow">已有试片</p><video controls preload="metadata" src={`/api/videos/${shotVideo.id}/content`} /></div>}{shotVideoTask && <div><p className="eyebrow">视频任务</p><div className="task-summary"><span className={`status status-${shotVideoTask.status}`}>{taskLabels[shotVideoTask.status]}</span><p>{shotVideoTask.provider} · {shotVideoTask.progress}%</p><small>{formatTime(shotVideoTask.updated_at)}</small></div></div>}</section>}
      <section className="review-box"><label className="field"><span>审核意见</span><textarea value={note} onChange={(event) => setNote(event.target.value)} maxLength="2000" placeholder="角色、动作、构图、提示词需要怎样调整…" /></label><div className="review-actions"><button className="ghost" disabled={busy} onClick={() => review('pending')}>恢复待审</button><button className="ghost changes" disabled={busy} onClick={() => review('changes_requested')}>要求修改</button><button className="ghost danger" disabled={busy} onClick={() => review('rejected')}>驳回</button><button className="primary approve" disabled={busy} onClick={() => review('approved')}>通过镜头</button></div></section>
      <section className="generation-bar"><div><strong>分镜图</strong><small>独立创建首帧图、末帧图、预览图；不生成视频。</small></div><button className="ghost" disabled={busy || !config?.openai_image_configured || hasActiveImageTask} onClick={generateImages}>{hasActiveImageTask ? '分镜图生成中' : '生成/补全分镜图'}</button></section>
      <section className="generation-bar"><div><strong>{canGenerate ? '镜头已过视频生成门禁' : '先通过审核与路线门禁'}</strong><small>视频生成与分镜图独立。MiniMax 需单独确认。</small></div><div className="generation-options"><label><input type="checkbox" checked={useFrameImages} disabled={referenceImageIDs.length > 0} onChange={(event) => setUseFrameImages(event.target.checked)} /> 首尾帧作为视频输入</label><label>角色模型写入提示词<select value={characterPromptCount} onChange={(event) => setCharacterPromptCount(Number(event.target.value))}><option value={0}>不写入</option><option value={1}>1 个</option><option value={2}>2 个</option><option value={3}>3 个</option></select></label><label>参考图资源（最多 9 张，和首尾帧互斥）<select multiple value={referenceImageIDs} disabled={useFrameImages} onChange={(event) => setReferenceImageIDs([...event.target.selectedOptions].map((option) => option.value).slice(0, 9))}>{referenceImages.map((asset) => <option key={asset.id} value={asset.id}>{asset.name}</option>)}</select></label></div><div className="row"><button className="ghost" disabled={busy || !canGenerate || ['queued', 'running'].includes(shotVideoTask?.status)} onClick={() => generate('mock')}>Mock 视频预演</button><button className="primary" disabled={busy || !canGenerate || !config?.paid_generation_allowed || !config?.minimax_configured || ['queued', 'running'].includes(shotVideoTask?.status)} onClick={() => generate('minimax')}>生成视频</button></div></section>
    </article> : <section className="empty">没有分镜。先导入分镜与提示词。</section>}
    {linking && <div className="modal-backdrop" role="presentation" onMouseDown={() => setLinking(false)}><form className="modal stack-form" onSubmit={linkAsset} onMouseDown={(event) => event.stopPropagation()}><div className="section-head"><div><p className="eyebrow">Shot assets</p><h2>关联到 {selected.id}</h2></div><button type="button" className="ghost" onClick={() => setLinking(false)}>关闭</button></div><label className="field"><span>素材</span><select name="asset_id" required defaultValue=""><option value="" disabled>选择素材</option>{assets.filter((asset) => !linkedAssets.some((item) => item.asset.id === asset.id)).map((asset) => <option value={asset.id} key={asset.id}>{kindLabels[asset.kind]} · {asset.name}</option>)}</select></label><label className="field"><span>说明</span><input name="note" maxLength="500" placeholder="角色定妆、场景气氛、道具参考…" /></label><button className="primary" disabled={busy}>保存关联</button></form></div>}
    {imageViewer && <ImageLightbox viewer={imageViewer} onClose={() => setImageViewer(null)} onSelect={(index) => setImageViewer((current) => ({ ...current, index }))} />}
  </section>
}

function Assets({ projectID, items, shots, videos, tasks, config, busy, run, reload }) {
  const [kind, setKind] = useState('')
  const [query, setQuery] = useState('')
  const [preview, setPreview] = useState(null)
  const [previewContent, setPreviewContent] = useState('')
  const [linking, setLinking] = useState(null)
  const [imageViewer, setImageViewer] = useState(null)
  const filtered = items.filter((item) => (!kind || item.kind === kind) && (!query || `${item.name} ${item.filename}`.toLowerCase().includes(query.toLowerCase())))
  const characterAssets = items.filter((item) => item.kind === 'character')

  function characterImages(item) {
    const order = ['预览图', '正面效果图', '侧面效果图', '动作效果图']
    return items.filter((image) => image.content_type?.startsWith('image/') && image.links?.some((link) => link.target_type === 'asset' && link.target_id === item.id)).sort((a, b) => order.findIndex((role) => a.name.endsWith(role)) - order.findIndex((role) => b.name.endsWith(role)))
  }

  async function generateCharacterImages() {
    const roles = 4
    const existing = characterAssets.reduce((total, item) => total + characterImages(item).length, 0)
    const pending = Math.max(0, characterAssets.length * roles - existing)
    const warning = `此操作会把角色图片加入 GPT Image 2 队列。\n\n待补角色：${characterAssets.length}\n待入队图片：最多 ${pending} 张（每个角色 1 张预览图 + 3 张效果图，已有任务或成品会跳过）\n输入版本：01_细分拆解小说资源/v003\n目标目录：/data/media/${projectID}/assets/<task_id>/\n\n确认继续？`
    if (!window.confirm(warning)) return
    await run(async () => { await api('/api/assets/characters/images/generate', json('POST', { project_id: projectID })); await reload() }).catch(() => {})
  }

  async function upload(event) {
    event.preventDefault()
    const formElement = event.currentTarget
    const form = new FormData(formElement)
    form.set('project_id', projectID)
    await run(async () => { await api('/api/assets', { method: 'POST', body: form }); formElement.reset(); await reload() }).catch(() => {})
  }
  async function remove(item) {
    if (!window.confirm(`删除素材“${item.name}”及文件？此操作不可恢复。`)) return
    await run(async () => { await api(`/api/assets/${item.id}`, { method: 'DELETE' }); await reload() }).catch(() => {})
  }
  async function openPreview(item) {
    await run(async () => { const data = await api(`/api/assets/${item.id}/preview`); setPreview(item); setPreviewContent(data.content) }).catch(() => {})
  }
  async function createLink(event) {
    event.preventDefault()
    const form = new FormData(event.currentTarget)
    const [targetType, targetID] = form.get('target').split(':')
    await run(async () => { await api(`/api/assets/${linking.id}/links`, json('POST', { target_type: targetType, target_id: targetID, note: form.get('note') })); setLinking(null); await reload() }).catch(() => {})
  }
  async function removeLink(item, link) {
    await run(async () => { await api(`/api/assets/${item.id}/links/${link.id}`, { method: 'DELETE' }); await reload() }).catch(() => {})
  }
  function linkLabel(link) {
    if (link.target_type === 'shot') return shots.find((shot) => shot.id === link.target_id)?.id || '已删除镜头'
    if (link.target_type === 'video') return videos.find((video) => video.id === link.target_id)?.title || '已删除试片'
    return items.find((asset) => asset.id === link.target_id)?.name || '已删除素材'
  }

  return <section className="workspace">
    <form className="panel upload-bar" onSubmit={upload}><label className="field"><span>素材名</span><input name="name" required maxLength="200" placeholder="角色定妆 / 场景参考" /></label><label className="field"><span>类型</span><select name="kind" defaultValue="image">{Object.entries(kindLabels).map(([value, label]) => <option value={value} key={value}>{label}</option>)}</select></label><label className="field file-field"><span>文件</span><input name="file" type="file" required /></label><button className="primary" disabled={busy}>上传素材</button></form>
    {characterAssets.length > 0 && <div className="panel character-generation"><div><p className="eyebrow">Character image queue</p><h2>角色预览图与效果图</h2><p>每个角色生成 1 张预览图、正面效果图、侧面效果图、动作效果图；已有任务或成品自动跳过。</p></div><button className="primary" disabled={busy || !config?.openai_image_configured} onClick={generateCharacterImages}>{config?.openai_image_configured ? `补全 ${characterAssets.length} 个角色图片` : 'GPT Image 2 未配置'}</button></div>}
    <div className="panel asset-toolbar"><div className="category-tabs"><button className={!kind ? 'active' : ''} onClick={() => setKind('')}>全部 {items.length}</button>{Object.entries(kindLabels).map(([value, label]) => <button className={kind === value ? 'active' : ''} key={value} onClick={() => setKind(value)}>{label} {items.filter((item) => item.kind === value).length}</button>)}</div><input value={query} onChange={(event) => setQuery(event.target.value)} placeholder="搜索素材" /></div>
    <div className="card-grid">{filtered.map((item) => {
      const previews = item.kind === 'character' ? characterImages(item) : []
      const active = tasks.filter((task) => task.asset_id === item.id && ['queued', 'running'].includes(task.status)).length
      const imageItems = previews.length > 0
        ? previews.map((image) => ({ id: image.id, src: `/api/assets/${image.id}/content`, label: image.name.split(' · ').pop(), alt: image.name }))
        : item.content_type.startsWith('image/') ? [{ id: item.id, src: `/api/assets/${item.id}/content`, label: item.name, alt: item.name }] : []
      const openImage = (index = 0) => setImageViewer({ title: item.name, images: imageItems, index })
      return <article className="media-card" key={item.id}>
        {imageItems.length > 0 ? <button type="button" className="asset-preview asset-preview-button" aria-label={`大图预览${item.name}`} onClick={() => openImage()}><img src={imageItems[0].src} alt={`${item.name} 预览`} /><span className="preview-hint">查看大图{imageItems.length > 1 && ` · ${imageItems.length} 张`}</span></button> : <div className="asset-preview">{item.content_type.startsWith('audio/') ? <audio controls src={`/api/assets/${item.id}/content`} /> : <span>{kindLabels[item.kind]}</span>}</div>}
        <div className="media-body"><div className="row between"><span className="tag">{kindLabels[item.kind]}</span><small>{item.kind === 'character' ? `${previews.length}/4 张图${active ? ` · ${active} 项生成中` : ''}` : `${item.links?.filter((link) => link.target_type === 'shot').length || 0} 个镜头`}</small></div><h3>{item.name}</h3><p title={item.filename}>{item.filename}</p><small>{formatBytes(item.size)} · {formatTime(item.created_at)}</small>{previews.length > 0 && <div className="character-effects">{imageItems.map((image, index) => <button type="button" key={image.id} aria-label={`大图预览${image.label}`} onClick={() => openImage(index)}><img src={image.src} alt={image.alt} /><span>{image.label}</span></button>)}</div>}{item.links?.length > 0 && <div className="links">{item.links.map((link) => <span className="link-chip" key={link.id}>{link.target_type === 'shot' ? '镜头' : link.target_type === 'video' ? '试片' : '素材'} · {linkLabel(link)}<button aria-label="移除关联" onClick={() => removeLink(item, link)}>×</button></span>)}</div>}</div>
        <div className="media-actions">{isTextAsset(item) && <button className="ghost" onClick={() => openPreview(item)}>打开</button>}<button className="ghost" onClick={() => setLinking(item)}>关联</button><button className="danger ghost" onClick={() => remove(item)}>删除</button></div>
      </article>
    })}{!filtered.length && <section className="empty"><h2>没有匹配素材</h2></section>}</div>
    {preview && <div className="modal-backdrop" role="presentation" onMouseDown={() => setPreview(null)}><section className="modal preview-modal" role="dialog" aria-modal="true" onMouseDown={(event) => event.stopPropagation()}><div className="section-head"><div><p className="eyebrow">Resource viewer</p><h2>{preview.name}</h2><p>{preview.filename}</p></div><button className="ghost" onClick={() => setPreview(null)}>关闭</button></div><pre className="resource-content">{previewContent}</pre></section></div>}
    {linking && <div className="modal-backdrop" role="presentation" onMouseDown={() => setLinking(null)}><form className="modal stack-form" onSubmit={createLink} onMouseDown={(event) => event.stopPropagation()}><div className="section-head"><div><p className="eyebrow">Resource link</p><h2>关联：{linking.name}</h2></div><button type="button" className="ghost" onClick={() => setLinking(null)}>关闭</button></div><label className="field"><span>目标</span><select name="target" required defaultValue=""><option value="" disabled>选择镜头、素材或试片</option><optgroup label="镜头">{shots.map((shot) => <option key={shot.id} value={`shot:${shot.id}`}>{shot.id} · {shot.visual}</option>)}</optgroup><optgroup label="素材">{items.filter((item) => item.id !== linking.id).map((item) => <option key={item.id} value={`asset:${item.id}`}>{item.name}</option>)}</optgroup><optgroup label="试片">{videos.map((video) => <option key={video.id} value={`video:${video.id}`}>{video.title}</option>)}</optgroup></select></label><label className="field"><span>关联说明</span><input name="note" maxLength="500" /></label><button className="primary" disabled={busy}>保存关联</button></form></div>}
    {imageViewer && <ImageLightbox viewer={imageViewer} onClose={() => setImageViewer(null)} onSelect={(index) => setImageViewer((current) => ({ ...current, index }))} />}
  </section>
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
    if (!window.confirm(`删除试片“${item.title}”及文件？此操作不可恢复。`)) return
    await run(async () => { await api(`/api/videos/${item.id}`, { method: 'DELETE' }); await reload() }).catch(() => {})
  }
  return <section className="workspace"><form className="panel upload-bar video-upload" onSubmit={upload}><label className="field"><span>试片名</span><input name="name" required maxLength="200" placeholder="E001-S001-SH001 初稿" /></label><label className="field file-field"><span>视频文件</span><input name="file" type="file" accept="video/*" required /></label><button className="primary" disabled={busy}>上传试片</button></form><div className="video-grid">{items.map((item) => <article className="video-card" key={item.id}><video controls preload="metadata" src={`/api/videos/${item.id}/content`} /><div className="media-body"><div className="row"><span className="tag">{item.provider || 'upload'}</span>{item.shot_id && <span className="tag muted-tag">{item.shot_id}</span>}</div><h3>{item.title}</h3><p>{item.filename}</p><small>{formatBytes(item.size)} · {formatTime(item.created_at)}</small></div><div className="media-actions"><a className="ghost" href={`/api/videos/${item.id}/content`} download={item.filename}>下载</a><button className="danger ghost" onClick={() => remove(item)}>删除</button></div></article>)}{!items.length && <section className="empty"><h2>没有试片</h2><p>审核镜头后创建预演或生成任务。</p></section>}</div></section>
}

function Tasks({ items, busy, run, reload }) {
  async function action(item, name) {
    const warning = name === 'retry' && item.provider === 'minimax' ? '重试可能再次产生 MiniMax 费用。确认继续？' : name === 'cancel' && item.provider === 'minimax' ? '取消只停止本地轮询，MiniMax H3 V2 远端生成不会停止。确认继续？' : null
    if (warning && !window.confirm(warning)) return
    await run(async () => { await api(`/api/tasks/${item.id}/${name}`, { method: 'POST' }); await reload() }).catch(() => {})
  }
  return <section className="workspace"><div className="panel"><div className="section-head"><div><p className="eyebrow">Task center</p><h2>执行任务</h2><p className="muted">镜头与角色生成入口分别在分镜审核页、素材库。这里负责状态、日志、取消和重试。</p></div><button className="ghost" onClick={reload}>刷新</button></div><div className="task-list">{items.map((item) => <article className="task-card" key={item.id}><div className="row between"><div className="row"><span className={`status status-${item.status}`}>{taskLabels[item.status]}</span><span className="tag">{item.provider}</span>{item.shot_id && <span className="tag muted-tag">{item.shot_id}</span>}{item.asset_id && <span className="tag muted-tag">角色素材 · {item.asset_id}</span>}</div><small>{formatTime(item.updated_at)}</small></div><h3>{item.prompt}</h3><p>{item.model} · {item.image_role || `${item.duration}s`} · {item.aspect_ratio}</p><div className="progress"><span style={{ width: `${item.progress}%` }} /></div><div className="row between"><small>{item.progress}% · 尝试 {item.attempts}/{item.max_attempts}</small><div className="row">{['queued', 'running'].includes(item.status) && <button className="danger ghost compact" disabled={busy} onClick={() => action(item, 'cancel')}>取消</button>}{['failed', 'cancelled'].includes(item.status) && <button className="ghost compact" disabled={busy} onClick={() => action(item, 'retry')}>重试</button>}</div></div>{item.error && <p className="task-error">{item.error}</p>}{!!item.logs?.length && <details><summary>日志 {item.logs.length}</summary><pre>{item.logs.join('\n')}</pre></details>}</article>)}{!items.length && <p className="muted">没有任务。</p>}</div></div></section>
}
