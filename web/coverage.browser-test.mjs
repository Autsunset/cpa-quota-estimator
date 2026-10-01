import {spawn} from 'node:child_process';
import fs from 'node:fs/promises';
import assert from 'node:assert/strict';

const origin = process.argv[2];
const chromePath = process.argv[3];
const root = process.argv[4];
assert.equal(typeof WebSocket, 'function', 'Node.js 22 or newer is required');
const profile = await fs.mkdtemp(root + '/chrome-');
const chrome = spawn(chromePath, ['--headless=new', '--no-sandbox', '--disable-gpu', '--no-first-run', '--no-default-browser-check', '--remote-debugging-port=0', '--user-data-dir=' + profile, 'about:blank'], {stdio: 'ignore'});
const pause = ms => new Promise(resolve => setTimeout(resolve, ms));
let socket;
try {
  let port;
  for (let i=0; i<100; i++) {
    try { port = (await fs.readFile(profile + '/DevToolsActivePort','utf8')).split('\n')[0]; break; } catch {}
    await pause(100);
  }
  assert(port, 'Chrome did not start');
  const pages = await (await fetch('http://127.0.0.1:' + port + '/json/list')).json();
  socket = new WebSocket(pages.find(page => page.type === 'page').webSocketDebuggerUrl);
  await new Promise(resolve => socket.addEventListener('open', resolve, {once:true}));
  let nextID = 0;
  const pending = new Map();
  const errors = [];
  socket.addEventListener('message', event => {
    const message = JSON.parse(event.data);
    if (message.method === 'Runtime.exceptionThrown') errors.push(message.params.exceptionDetails);
    if (message.id && pending.has(message.id)) {
      const {resolve,reject,timer} = pending.get(message.id);
      clearTimeout(timer);
      pending.delete(message.id);
      message.error ? reject(new Error(JSON.stringify(message.error))) : resolve(message.result);
    }
  });
  const send = (method,params={}) => new Promise((resolve,reject) => {
    const id=++nextID;
    const timer=setTimeout(()=>{pending.delete(id);reject(new Error('CDP request timed out: '+method));},5000);
    pending.set(id,{resolve,reject,timer});
    socket.send(JSON.stringify({id,method,params}));
  });
  const evaluate = async expression => {
    const result = await send('Runtime.evaluate',{expression,returnByValue:true,awaitPromise:true});
    if (result.exceptionDetails) throw new Error(result.exceptionDetails.exception?.description || result.exceptionDetails.text);
    return result.result.value;
  };
  const wait = async (expression,label) => {
    for (let i=0;i<150;i++) {
      if (await evaluate(expression)) return;
      await pause(100);
    }
    throw new Error('Timed out: ' + label + '\n' + await evaluate('document.body.innerText.slice(-1600)'));
  };
  const visible = id => evaluate(`!!document.getElementById(${JSON.stringify(id)})?.getClientRects().length`);
  const select = async (id,value) => evaluate(`document.getElementById(${JSON.stringify(id)}).value=${JSON.stringify(value)};document.getElementById(${JSON.stringify(id)}).dispatchEvent(new Event('change',{bubbles:true}));true`);
  const saveMode = async mode => {
    await evaluate("setWorkbenchView('settings', false);true");
    await select('coverageMode', mode);
    await evaluate("document.querySelector('#saveCoverageSettings').click();true");
    await wait(`state.collection_coverage.mode===${JSON.stringify(mode)} && !coverageSaving && $('#coverageSaveStatus').textContent.includes(language==='en'?'saved':'已保存')`, 'save ' + mode);
  };
  const screenshot = async name => {
    const result = await send('Page.captureScreenshot',{format:'png'});
    await fs.writeFile(root + '/' + name + '.png',Buffer.from(result.data,'base64'));
  };
  await send('Page.enable');
  await send('Runtime.enable');
  await send('Emulation.setDeviceMetricsOverride',{width:1365,height:1100,deviceScaleFactor:1,mobile:false});
  const keyScript = await send('Page.addScriptToEvaluateOnNewDocument',{source:"localStorage.setItem('cqe-persistent-key','local-browser-test');localStorage.setItem('cqe-language-mode','zh-CN');"});
  await send('Page.navigate',{url:origin});
  await wait("typeof state!=='undefined' && state?.account==='a-mixed' && seriesState && $('#fullTokens').textContent!=='—' && $('#monthTokens').textContent!=='—'", 'initial data');
  await screenshot('workbench-first-desktop');
  assert.equal(await evaluate("$('#coverageMode').value"),'cpa_only');
  assert((await evaluate("$('#coverageExplanation').textContent")).includes('假设全部用量经过 CPA'));
  assert((await evaluate("$('#note').textContent")).includes('2 条用量记录'));
  assert.equal(await evaluate("document.querySelectorAll('input[name=pricingMode]').length"),3);
  assert.equal(await evaluate("document.querySelector('input[name=pricingMode]:checked').value"),'credits');
  assert.equal(await evaluate("$('#pricingTitle').textContent"),'计价口径');
  assert.equal(await evaluate("$('#workbench').dataset.view"),'overview');
  assert.equal(await evaluate("$('#wbAccountList').querySelectorAll('.wb-account-item').length"),3);
  await evaluate("$('#wbAccountSearch').focus();$('#wbAccountSearch').dispatchEvent(new KeyboardEvent('keydown',{key:'ArrowDown',bubbles:true}));true");
  assert.equal(await evaluate("document.activeElement.classList.contains('wb-account-item')"),true,'account search enters list by keyboard');
  await evaluate("document.querySelector('[data-wb-view=overview]').focus();document.querySelector('[data-wb-view=overview]').dispatchEvent(new KeyboardEvent('keydown',{key:'ArrowRight',bubbles:true}));true");
  assert.equal(await evaluate("$('#workbench').dataset.view"),'usage','arrow key switches tabs');
  await evaluate("document.querySelector('[data-wb-view=overview]').click();true");
  await evaluate("$('#wbAccountList button[title=\"waiting-cpa\"]').click();true");
  await wait("state.account==='waiting-cpa' && !state.latest && $('#wbFeedback').dataset.state==='good'",'real zero-sample account');
  assert.equal(await visible('wbEmpty'),true,'waiting account explains empty state');
  assert.equal(await visible('paceCurve'),false,'waiting account cannot show prior account forecast');
  await screenshot('workbench-waiting-desktop');
  await evaluate("$('#wbAllAccounts').click();true");
  assert.equal(await visible('wbEmpty'),false,'account list does not retain detail empty state');
  await evaluate("$('#wbAccountList button[title=\"a-mixed\"]').click();true");
  await wait("state.account==='a-mixed' && !!state.latest",'return from waiting account');
  await evaluate("$('#wbAccountSearch').value='no-such-account';$('#wbAccountSearch').dispatchEvent(new Event('input',{bubbles:true}));true");
  assert((await evaluate("$('#wbAccountList').textContent")).includes('没有匹配的账号'));
  await evaluate("$('#wbAccountSearch').value='';$('#wbAccountSearch').dispatchEvent(new Event('input',{bubbles:true}));true");
  await evaluate("document.dispatchEvent(new KeyboardEvent('keydown',{key:'k',ctrlKey:true,bubbles:true}));true");
  assert.equal(await evaluate("$('#wbCommand').open"),true,'quick account search opens');
  await evaluate("$('#wbCommandSearch').value='b-cpa';$('#wbCommandSearch').dispatchEvent(new Event('input',{bubbles:true}));true");
  assert.equal(await evaluate("$('#wbCommandList').querySelectorAll('button').length"),1);
  await evaluate("$('#wbCommandList button').click();true");
  await wait("state.account==='b-cpa' && $('#workbench').dataset.view==='overview'",'command account selection');
  await evaluate("$('#wbAccountList button[title=\"a-mixed\"]').click();true");
  await wait("state.account==='a-mixed'",'sidebar account selection');
  await evaluate("history.back();true");
  await wait("state.account==='b-cpa'",'browser back restores previous account');
  await evaluate("history.forward();true");
  await wait("state.account==='a-mixed'",'browser forward restores selected account');
  assert((await evaluate("$('#pricePreviewRows').children.length"))>=6,'price preview has model rows');
  assert.equal(await evaluate("[...$('#pricePreviewRows').children].some(row=>row.firstElementChild.textContent.startsWith('gpt-6.1-sol'))"),true,'GPT-6.1 Sol is visible in price preview');
  assert.deepEqual(await evaluate("(()=>{let row=[...$('#pricePreviewRows').children].find(row=>row.firstElementChild.textContent.startsWith('gpt-6.1-sol'));return ['input','cache_read','output'].map(field=>row.querySelector('[data-price-field='+field+']').dataset.calibrationStatus);})()"),['prior','calibrated','prior'],'only the calibrated component receives that status');
  const priorPriceText = await evaluate("[...$('#pricePreviewRows').children].find(row=>row.firstElementChild.textContent.startsWith('gpt-6.1-sol')).querySelector('[data-price-field=input]').textContent");
  assert(priorPriceText.includes('区间 [') && priorPriceText.includes('未标定'), 'uncalibrated prices retain a visible prior interval');
  assert(!priorPriceText.includes('±0.0%'), 'an uncalibrated price must not claim zero uncertainty');
  const referencePriceStatuses = await evaluate("(()=>{let row=[...$('#pricePreviewRows').children].find(row=>row.firstElementChild.textContent.startsWith('gpt-5.6-sol'));return ['input','cache_read','output'].map(field=>row.querySelector('[data-price-field='+field+']').dataset.calibrationStatus);})()");
  assert.deepEqual(referencePriceStatuses,['baseline','prior','prior'],'the input unit convention does not calibrate reference cache/output rates');
  assert.deepEqual(await evaluate("(()=>{let row=[...$('#pricePreviewRows').children].find(row=>row.firstElementChild.textContent.startsWith('gpt-6.1-sol'));return ['input','cache_read','output'].map(field=>row.querySelector('[data-price-field='+field+'] b').textContent);})()"),['50 credits','4 credits','250 credits'],'a cache adjustment leaves input and output at official rates');
  assert.equal(await evaluate("[...$('#pricePreviewRows').children].some(row=>row.firstElementChild.textContent.startsWith('gpt-5.4'))"),false,'retired model hidden from price preview');
  assert(await evaluate("$('#account').getBoundingClientRect().width<=185 && $('#refresh').getBoundingClientRect().height===$('#wbSearchTrigger').getBoundingClientRect().height"),'toolbar control sizes');
  await evaluate("$('#refresh').click();true");
  await wait("$('#toolbarStatus').textContent.includes('已重新读取页面数据')",'refresh feedback');
  assert.equal(await evaluate("document.getElementById('astraMultiplier')===null"),true);
  await evaluate("document.querySelector('[data-wb-view=pricing]').click();true");
  assert.equal(await visible('pricingTitle'),true);
  assert.equal(await visible('guidedCalibration'),true);
  await evaluate("$('#sync').click();true");
  await wait("$('#toolbarStatus').textContent.includes('已同步 1 个模型价格')",'pricing sync feedback');
  await screenshot('workbench-pricing-desktop');
  const pooledRendering = await evaluate(`(() => {
    renderWeightCard({available:true,segment_count:25,lag:2,fitted_at:1800000000,
      fast:{source:'prior',value:2.5,low:0.94,high:6.66,prior_locked:true},
      long_context:{source:'prior',value:1,low:0.38,high:2.66,prior_locked:true},
      fast_long:{source:'provisional',value:2.856,low:1.4,high:5.8,identified:false,data_share:0.2},
      models:[{model:'gpt-5.6-sol',input:{source:'anchor',value:1,low:1,high:1,prior_locked:true},cache:{source:'prior',value:0.1,low:0.038,high:0.266,prior_locked:true},output:{source:'prior',value:5,low:1.9,high:13.3,prior_locked:true}}],
      pooled_models:[{model:'gpt-6.1-sol',estimate:{source:'pooled',value:1.4,low:1.2,high:1.6,identified:true},applied:false,
        evidence:{validation_segments:5,prior_mae:0.2,candidate_mae:0.3,accepted:false},
        composition:{input:{tokens:100,mean_share:0.2},cache:{tokens:1000,mean_share:0.5},output:{tokens:50,mean_share:0.3}}}],
      guidance:[{model:'completed-model',action:'model_contrast',priority:100,completed:true},{model:'gpt-6.1-sol',action:'cache_contrast',priority:2,completed:false}]},{});
    return {pooled:$('#weightPooled').textContent,weights:$('#weightRows').textContent,action:$('#weightAction').textContent,combined:$('#weightCombined').textContent,modifiers:$('#weightSummary').textContent,applied:$('#weightPooled [data-pooled-model]').dataset.applied};
  })()`);
  assert.equal(pooledRendering.applied,'false');
  assert(pooledRendering.pooled.includes('尚未应用') && pooledRendering.pooled.includes('0.200 → 0.300'), 'failed validation remains visibly inactive');
  assert(pooledRendering.weights.includes('基准约定，非实测标定') && pooledRendering.weights.includes('官方先验'), 'weight sources distinguish conventions and priors');
  assert(pooledRendering.action.includes('gpt-6.1-sol') && pooledRendering.action.includes('缓存') && !pooledRendering.action.includes('completed-model'), 'guidance skips completed work');
  assert(pooledRendering.combined.includes('2.86') && pooledRendering.combined.includes('暂估') && pooledRendering.combined.includes('不代表两个单项'), 'a combined provisional estimate is not presented as independent modifier calibration');
  assert(pooledRendering.modifiers.includes('官方先验'), 'standalone modifiers retain their own prior labels');
  await evaluate("$('#weightPooled').scrollIntoView({block:'center'});true");
  await screenshot('pooled-validation-desktop');
  await evaluate('renderWeightCard({},{});true');
  assert.equal(await visible('weightCombined'),false,'clearing a fit also clears its combined-mode estimate');
  await evaluate("document.querySelector('input[name=pricingMode][value=custom]').click();true");
  assert.equal(await visible('customPriceEditor'),true);
  assert((await evaluate("$('#customPriceRows').children.length"))>=6,'custom editor has model rows');
  await evaluate("pricingSettingsDirty=false;document.querySelector('input[name=pricingMode][value=credits]').click();pricingSettingsDirty=false;renderPricingSettings(state.config,priceCatalogState);true");
  await evaluate("document.querySelector('[data-wb-view=overview]').click();true");
  const originalTokens = await evaluate("$('#tokens').textContent");
  const originalQuota = await evaluate("$('#used').textContent");
  const originalCapacity = await evaluate("$('#fullTokens').textContent");
  assert(await visible('fullTokens'));
  await screenshot('coverage-default-desktop');
  await evaluate("$('#wbAllAccounts').click();true");
  assert.equal(await visible('overviewRows'),true,'full account comparison opens');
  await screenshot('workbench-accounts-desktop');
  await evaluate("document.querySelector('#overviewRows tr.sampled').click();true");
  assert.equal(await evaluate("$('#workbench').dataset.view"),'overview');
  await evaluate("document.querySelector('[data-wb-view=history]').click();true");
  await evaluate("$('#showSpark').checked=true;$('#showSpark').dispatchEvent(new Event('change',{bubbles:true}));true");
  await wait("seriesState?.spark_weekly_quota && document.getElementById('sparkWeeklyFullTokens')", 'Spark scopes');
  await screenshot('workbench-history-desktop');
  await saveMode('mixed');
  await evaluate("setWorkbenchView('history', false);true");
  for (const id of ['capacityCurve','weeklyFullTokens','weeklyAllowanceRows','sparkFullTokens','sparkWeeklyFullTokens','monthCapacity','weeklyMonthCapacity','sparkMonthCapacity','sparkWeeklyMonthCapacity']) {
    assert.equal(await visible(id),false, id + ' should be hidden in mixed mode');
  }
  for (const id of ['weeklyUsed','sparkUsed','sparkWeeklyUsed','monthTokens']) {
    assert.equal(await visible(id),true,id + ' observations should remain visible');
  }
  await evaluate("setWorkbenchView('usage', false);true");
  assert.equal(await visible('allowanceRows'),false);
  await evaluate("setWorkbenchView('overview', false);true");
  assert.equal(await visible('fullTokens'),false);
  for (const id of ['tokens','used','paceCurve']) assert.equal(await visible(id),true,id + ' observation visible');
  assert.equal(await evaluate("$('#tokens').textContent"),originalTokens);
  assert.equal(await evaluate("$('#used').textContent"),originalQuota);
  assert((await evaluate("$('#coverageSampleQuality').textContent")).includes('高'));
  assert.equal(await evaluate("seriesState.estimate.confidence"),'unavailable');
  await screenshot('coverage-mixed-desktop');
  await select('account','b-cpa');
  await wait("state.account==='b-cpa' && seriesState.account==='b-cpa' && !document.body.classList.contains('collection-limited')",'unaffected account');
  assert.equal(await evaluate("$('#coverageMode').value"),'cpa_only');
  assert(await visible('fullTokens'));
  const previousTimeOrigin = await evaluate('performance.timeOrigin');
  await send('Page.reload');
  await wait("typeof state!=='undefined' && performance.timeOrigin!=="+previousTimeOrigin+" && state?.account==='b-cpa' && seriesState",'selected account persists across reload');
  await evaluate("$('#wbAccountList button[title=\"a-mixed\"]').click();true");
  await wait("state.account==='a-mixed' && document.body.classList.contains('collection-limited')",'persisted mixed mode');
  assert.equal(await evaluate("$('#coverageMode').value"),'mixed');
  await saveMode('unknown');
  assert.equal(await evaluate('seriesState.estimate.unavailable_reason'),'usage_coverage_unknown');
  await send('Emulation.setDeviceMetricsOverride',{width:390,height:844,deviceScaleFactor:1,mobile:true});
  await evaluate("setWorkbenchView('accounts', false);true");
  assert.equal(await visible('wbSidebar'),true,'mobile account list');
  await screenshot('workbench-mobile-accounts');
  await evaluate("$('#wbAccountList button[title=\"b-cpa\"]').click();true");
  await wait("state.account==='b-cpa' && $('#workbench').dataset.view==='overview'",'mobile detail');
  assert(await evaluate("document.documentElement.scrollWidth<=innerWidth+1"),'mobile detail does not overflow');
  await screenshot('workbench-mobile-overview');
  await evaluate("$('#wbBack').click();true");
  assert.equal(await visible('wbSidebar'),true,'mobile back to accounts');
  await evaluate("$('#wbAccountList button[title=\"a-mixed\"]').click();true");
  await wait("state.account==='a-mixed'",'mobile return to sampled account');
  await evaluate("setWorkbenchView('settings', false);true");
  await evaluate("$('#coverageTitle').scrollIntoView({block:'start'});true");
  assert(await evaluate("[...document.querySelectorAll('.collection-controls select,.collection-controls button')].every(el=>{let r=el.getBoundingClientRect();return r.left>=0 && r.right<=innerWidth;})"),'mobile controls overflow');
  await screenshot('coverage-unknown-mobile');
  await select('language','en');
  await wait("document.documentElement.lang==='en' && $('#coverageExplanation').textContent.startsWith('Coverage unknown')",'English coverage strings');
  assert.equal(await evaluate("$('#pricingTitle').textContent"),'Pricing basis');
  assert.equal(await evaluate("document.querySelector('.price-preview h2').textContent"),'Price table');
  assert.equal(await evaluate("document.querySelector('.pricing-choice small').textContent.startsWith('Estimate from API rates')"),true);
  assert.equal(await evaluate("state.collection_coverage.mode"), 'unknown');
  await screenshot('coverage-unknown-mobile-en');
  await saveMode('cpa_only');
  assert.equal(await evaluate("$('#fullTokens').textContent"),originalCapacity);
  await evaluate("setWorkbenchView('overview', false);true");
  assert(await visible('fullTokens'));
  await send('Emulation.setDeviceMetricsOverride',{width:1365,height:1100,deviceScaleFactor:1,mobile:false});
  await evaluate("window.scrollTo(0,0);true");
  await screenshot('workbench-overview-en-doc');
  await fetch(origin + '/test/fail-refresh');
  await select('coverageMode','mixed');
  await evaluate("$('#saveCoverageSettings').click();true");
  await wait("!coverageSaving && $('#coverageSaveStatus').textContent.includes('data refresh failed')", 'saved setting with failed refresh');
  assert.equal(await evaluate('state.collection_coverage.mode'), 'mixed');
  await evaluate("setWorkbenchView('overview', false);true");
  assert.equal(await visible('fullTokens'),false);
  await evaluate("$('#resetRange').click();true");
  assert.equal(await visible('capacityCurve'),false,'cached chart must stay hidden after failed refresh');
  await evaluate("$('#refresh').click();true");
  await wait("seriesState.collection_coverage.mode==='mixed' && $('#coverageSaveStatus').textContent==='Account collection coverage saved'", 'refresh recovery');
  await saveMode('cpa_only');
  await fetch(origin + '/test/fail-write');
  await select('coverageMode','mixed');
  await evaluate("$('#saveCoverageSettings').click();true");
  await wait("!coverageSaving && $('#coverageSaveStatus').textContent.includes('injected save failure')",'failed save');
  assert.equal(await evaluate('state.collection_coverage.mode'),'cpa_only');
  await evaluate("setWorkbenchView('overview', false);true");
  assert(await visible('fullTokens'));
  await evaluate("setWorkbenchView('settings', false);true");
  await evaluate("$('#saveCoverageSettings').click();true");
  await wait("!coverageSaving && state.collection_coverage.mode==='mixed'",'retry save');
  await fetch(origin + '/test/delay-read');
  await evaluate("$('#refresh').click();true");
  assert.equal(await evaluate("$('#wbFeedback').dataset.state"),'loading','refresh exposes loading state');
  await select('account','b-cpa');
  await wait("state.account==='b-cpa' && seriesState.account==='b-cpa'",'account switch during read');
  await pause(600);
  assert.equal(await evaluate('state.account'),'b-cpa');
  assert.equal(await evaluate("$('#coverageMode').value"),'cpa_only');
  await evaluate("setWorkbenchView('pricing', false);true");
  await evaluate("$('#savePricingSettings').click();true");
  await wait("!$('#pricingTaskProgress').hidden",'background pricing task visible');
  await wait("$('#pricingTaskText').textContent.includes('Recalculation complete')",'background pricing task complete');
  assert.equal(await evaluate("$('#savePricingSettings').disabled"),false);
  await fetch(origin + '/test/fail-refresh');
  await evaluate("$('#refresh').click();true");
  await wait("$('#wbFeedback').dataset.state==='error' && !$('#wbRetry').hidden",'read error state');
  await evaluate("$('#wbRetry').click();true");
  await wait("$('#wbFeedback').dataset.state==='good'",'error retry');
  await send('Network.enable');
  await send('Network.emulateNetworkConditions',{offline:true,latency:0,downloadThroughput:0,uploadThroughput:0});
  await evaluate("$('#refresh').click();true");
  await wait("$('#wbFeedback').dataset.state==='offline'",'offline state');
  await send('Emulation.setDeviceMetricsOverride',{width:390,height:844,deviceScaleFactor:1,mobile:true});
  await evaluate("setWorkbenchView('accounts', false);true");
  assert.equal(await visible('wbSidebarError'),true,'mobile account list shows offline feedback');
  assert(await evaluate("$('#refresh').getBoundingClientRect().width===44 && !!$('#refresh').getAttribute('aria-label')"),'mobile reload control remains compact and named');
  await screenshot('workbench-mobile-offline');
  await send('Network.emulateNetworkConditions',{offline:false,latency:0,downloadThroughput:-1,uploadThroughput:-1});
  await wait("$('#wbFeedback').dataset.state==='good'",'online recovery');
  await evaluate("setWorkbenchView('overview', false);true");
  await evaluate("renderWorkbenchAccounts([], '');renderWorkbenchSummary({account:'empty-account',latest:null});true");
  assert((await evaluate("$('#wbAccountList').textContent")).includes('No accounts yet'));
  assert.equal(await visible('wbEmpty'),true,'empty state');
  await evaluate("load();true");
  await wait("$('#wbFeedback').dataset.state==='good' && document.querySelectorAll('#wbAccountList button').length===3",'empty state recovery');
  for (const width of [320,768,1024]) {
    await send('Emulation.setDeviceMetricsOverride',{width,height:900,deviceScaleFactor:1,mobile:width<761});
    await evaluate("setWorkbenchView('overview', false);true");
    assert(await evaluate("document.documentElement.scrollWidth<=innerWidth+1"),width+'px layout does not overflow');
  }
  await send('Emulation.setDeviceMetricsOverride',{width:1365,height:1100,deviceScaleFactor:1,mobile:false});
  await send('Emulation.setEmulatedMedia',{features:[{name:'prefers-color-scheme',value:'dark'}]});
  assert.equal(await evaluate("getComputedStyle(document.body).backgroundColor"),'rgb(20, 27, 27)');
  await wait("getComputedStyle($('#wbSearchTrigger')).backgroundColor==='rgb(34, 45, 43)' && getComputedStyle($('#wbAccountList .is-selected')).backgroundColor==='rgb(36, 59, 50)'",'dark controls');
  assert.deepEqual(JSON.parse(await evaluate("JSON.stringify([...document.querySelectorAll('body *')].filter(e=>e.children.length===0&&e.getClientRects().length&&/[\u3400-\u9fff]/.test(e.textContent)).map(e=>e.textContent.trim()))")),[],'English pricing view has no untranslated labels');
  await screenshot('workbench-dark-desktop');
  await send('Emulation.setEmulatedMedia',{features:[{name:'prefers-color-scheme',value:'dark'},{name:'prefers-reduced-motion',value:'reduce'}]});
  assert.equal(await evaluate("getComputedStyle($('#wbQuotaFill')).transitionProperty.includes('transform')"),false,'reduced motion removes quota movement');
  await send('Emulation.setEmulatedMedia',{features:[{name:'prefers-color-scheme',value:'light'},{name:'prefers-reduced-motion',value:'no-preference'}]});
  await evaluate("setWorkbenchView('overview', false);true");
  await wait("getComputedStyle($('#wbSearchTrigger')).backgroundColor==='rgb(251, 252, 250)'",'light theme settled');
  await screenshot('workbench-overview-en');
  for (const width of [320,375,390,430,768]) {
    await send('Emulation.setDeviceMetricsOverride',{width,height:844,deviceScaleFactor:1,mobile:width<761});
    for (const view of ['overview','usage','pricing','history','settings']) {
      await evaluate(`setWorkbenchView('${view}', false);window.scrollTo(0,0);true`);
      await pause(160);
      assert.equal(await evaluate('innerWidth'),width,'viewport is not enlarged by overflowing content');
      assert(await evaluate('document.documentElement.scrollWidth<=innerWidth+1'),width+'px '+view+' fits screen');
      if (width<761) assert(await evaluate("[...$('#wbTabs').children].every(el=>{let r=el.getBoundingClientRect();return r.left>=0&&r.right<=innerWidth&&r.height>=44;})"),'all phone tabs are visible and touch sized');
      if (width<761 && view==='pricing') {
        assert(await evaluate("$('#pricePreviewRows').closest('table').scrollWidth<= $('#pricePreviewRows').closest('table').clientWidth+1"),'price cards do not need horizontal scrolling');
        assert(await evaluate("[...$('#pricePreviewRows tr:first-child').cells].slice(1).every(cell=>cell.dataset.label.length>0)"),'price cards have field labels');
      }
      if (width<761 && view==='overview') {
        assert(await evaluate("$('#paceCurve svg').viewBox.baseVal.width <= $('#paceCurve').clientWidth+1"),'pace chart uses available width');
        assert(await evaluate("$('#paceCurve svg text').getBoundingClientRect().height>=9"),'mobile chart labels remain readable');
      }
    }
    if (width===390) {
      await evaluate("setWorkbenchView('overview', false);window.scrollTo(0,0);true");
      await pause(160);
      await screenshot('mobile-overview-readable');
      const tab = await evaluate("(()=>{let el=$('[data-wb-view=pricing]'),r=el.getBoundingClientRect();return {x:r.left+r.width/2,y:r.top+r.height/2};})()");
      await send('Input.dispatchTouchEvent',{type:'touchStart',touchPoints:[tab]});
      await send('Input.dispatchTouchEvent',{type:'touchEnd',touchPoints:[]});
      await wait("$('#workbench').dataset.view==='pricing'",'touch opens pricing tab');
      await screenshot('mobile-pricing-cards');
      await evaluate("$('#pricePreviewRows').scrollIntoView({block:'start'});true");
      await screenshot('mobile-price-details');
    }
  }
  await send('Emulation.setDeviceMetricsOverride',{width:390,height:844,deviceScaleFactor:1,mobile:true});
  await send('Page.navigate',{url:origin+'/mobile-host'});
  await wait("document.querySelector('iframe')?.contentWindow.eval(\"typeof state!=='undefined' && state?.account && $('#workbench').dataset.loading!=='true'\")",'embedded phone loads account');
  assert.equal(await evaluate("document.querySelector('iframe').contentWindow.eval(\"$('#workbench').dataset.view\")"),'overview','embedded phone opens account overview directly');
  assert(await evaluate("document.querySelector('iframe').contentDocument.documentElement.scrollWidth<=390"),'embedded phone fits iframe');
  await screenshot('mobile-embedded-overview');
  await send('Emulation.setDeviceMetricsOverride',{width:1365,height:1100,deviceScaleFactor:1,mobile:false});
  await send('Page.navigate',{url:origin});
  await wait("typeof state!=='undefined' && state?.account && $('#wbFeedback').dataset.state==='good'",'return from embedded phone');
  await select('language','en');
  await wait("document.documentElement.lang==='en'",'restore English after embedded navigation');
  await send('Page.removeScriptToEvaluateOnNewDocument',{identifier:keyScript.identifier});
  await evaluate("localStorage.removeItem('cqe-persistent-key');sessionStorage.removeItem('cqe-key');localStorage.removeItem('cli-proxy-auth');localStorage.removeItem('managementKey');true");
  await send('Page.reload');
  await wait("document.getElementById('login')?.classList.contains('show')",'login state');
  await screenshot('workbench-login-en');
  assert.equal(await evaluate("document.querySelector('label[for=key]')?.textContent"),'Management Key');
  await evaluate("$('#key').value='local-browser-test';$('#loginBtn').click();true");
  await wait("state?.account && !$('#login').classList.contains('show')",'login completion');
  assert.equal(errors.length,0,JSON.stringify(errors));
  console.log(JSON.stringify({workbenchNavigation:true,search:true,command:true,loading:true,empty:true,error:true,offline:true,dark:true,reducedMotion:true,login:true,defaultAssumption:true,mixedAndUnknown:true,independentScopes:true,persistence:true,accountIsolation:true,failedSaveRetry:true,failedRefreshProtection:true,staleReadProtection:true,mobile:true,english:true,pricingTask:true,javascriptErrors:errors.length}));
} finally {
  socket?.close();
  await new Promise(resolve => {
    if (chrome.exitCode !== null) return resolve();
    const timer=setTimeout(()=>chrome.kill('SIGKILL'),3000);
    chrome.once('exit',()=>{clearTimeout(timer);resolve();});
    chrome.kill('SIGTERM');
  });
}
