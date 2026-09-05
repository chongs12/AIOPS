import { createRouter, createWebHistory } from 'vue-router'
import AIOpsChat from './views/AIOpsChat.vue'
import TicketList from './views/TicketList.vue'
import TicketDetail from './views/TicketDetail.vue'
import Admin from './views/Admin.vue'

export default createRouter({
  history: createWebHistory(),
  routes: [
    { path: '/', component: AIOpsChat, meta: { title: 'AI 对话' } },
    { path: '/tickets', component: TicketList, meta: { title: '工作台' } },
    { path: '/tickets/:id', component: TicketDetail, meta: { title: '工单详情' } },
    { path: '/admin', component: Admin, meta: { title: '管理面' } },
  ],
})
