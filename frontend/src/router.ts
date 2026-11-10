import { createRouter, createWebHistory } from 'vue-router'

export const router = createRouter({
  history: createWebHistory(),
  routes: [
    { path: '/', redirect: '/rules' },
    { path: '/rules', component: () => import('./views/RulesView.vue') },
    { path: '/connections', component: () => import('./views/ConnectionsView.vue') },
    { path: '/activity', component: () => import('./views/ActivityView.vue') },
    // events and runs were pages of their own before they became one
    { path: '/events', redirect: '/activity' },
    { path: '/runs', redirect: (to) => ({ path: '/activity', query: to.query.id ? { run: to.query.id } : {} }) },
  ],
})
