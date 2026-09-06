import { createRouter, createWebHistory } from 'vue-router'
import AIOpsChat from './views/AIOpsChat.vue'
import Observability from './views/Observability.vue'
import MetricsQuery from './views/MetricsQuery.vue'
import LogsQuery from './views/LogsQuery.vue'
import EventWall from './views/EventWall.vue'
import Deployments from './views/Deployments.vue'
import TraceObservability from './views/TraceObservability.vue'
import TicketList from './views/TicketList.vue'
import TicketDetail from './views/TicketDetail.vue'
import Admin from './views/Admin.vue'
import Login from './views/Login.vue'

export default createRouter({
  history: createWebHistory(),
  routes: [
    { path: '/', component: AIOpsChat, meta: { title: 'AI 对话' } },
    { path: '/tickets', component: TicketList, meta: { title: '工作台' } },
    { path: '/tickets/:id', component: TicketDetail, meta: { title: '工单详情' } },
    { path: '/observability', component: Observability, meta: { title: '可观测总览' } },
    { path: '/metrics', component: MetricsQuery, meta: { title: '指标查询' } },
    { path: '/logs', component: LogsQuery, meta: { title: '日志查询' } },
    { path: '/events', component: EventWall, meta: { title: '事件墙' } },
    { path: '/trace', component: TraceObservability, meta: { title: '链路追踪' } },
    { path: '/deployments', component: Deployments, meta: { title: '部署管理' } },
    { path: '/admin', component: Admin, meta: { title: '管理面' } },
    { path: '/login', component: Login, meta: { title: '登录' } },
  ],
})
