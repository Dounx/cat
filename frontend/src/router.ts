import { createRouter, createWebHistory } from 'vue-router'

export const router = createRouter({
  history: createWebHistory(),
  routes: [
    { path: '/', redirect: '/events' },
    { path: '/events', component: () => import('./views/EventsView.vue') },
    { path: '/rules', component: () => import('./views/RulesView.vue') },
    { path: '/runs', component: () => import('./views/RunsView.vue') },
    { path: '/connections', component: () => import('./views/ConnectionsView.vue') },
  ],
})
