import { createRouter, createWebHistory } from 'vue-router'
import { useSession } from './stores/session'
import { Cap, type CapValue } from './lib/caps'
import { setTitleForRoute } from './lib/title'

// Every non-public route names the capability it needs. The server enforces the same one;
// this only stops us from routing someone to a page that would be a wall of 403s.
declare module 'vue-router' {
  interface RouteMeta {
    public?: boolean
    cap?: CapValue
  }
}

// Routes are code-split: the login page must not pull the container views in with it.
export const router = createRouter({
  history: createWebHistory(),
  routes: [
    {
      path: '/login',
      name: 'login',
      component: () => import('./views/LoginView.vue'),
      meta: { public: true },
    },
    {
      path: '/',
      component: () => import('./views/AppShell.vue'),
      children: [
        { path: '', redirect: '/overview' },
        // Reachable only by the guard below, for a signed-in user who holds no
        // capability at all. It needs no capability of its own, or it could not be shown
        // to the one person who needs it.
        {
          path: 'no-access',
          name: 'no-access',
          component: () => import('./views/NoAccessView.vue'),
        },
        // The front door. Landing on a list of stacks answers "what exists"; the question you
        // actually arrive with is "is anything wrong", and that used to take four clicks across
        // three pages to answer.
        {
          path: 'overview',
          name: 'overview',
          component: () => import('./views/OverviewView.vue'),
          meta: { cap: Cap.StacksView },
        },
        {
          path: 'services',
          name: 'services',
          component: () => import('./views/ServicesView.vue'),
          meta: { cap: Cap.ServicesView },
        },
        {
          path: 'services/:id',
          name: 'service',
          component: () => import('./views/ServiceView.vue'),
          meta: { cap: Cap.ServicesView },
        },
        {
          path: 'containers',
          name: 'containers',
          component: () => import('./views/ContainersView.vue'),
          meta: { cap: Cap.ContainersView },
        },
        {
          path: 'containers/:id',
          name: 'container',
          component: () => import('./views/ContainerView.vue'),
          meta: { cap: Cap.ContainersView },
        },
        {
          path: 'stacks',
          name: 'stacks',
          component: () => import('./views/StacksView.vue'),
          meta: { cap: Cap.StacksView },
        },
        {
          path: 'stacks/:id',
          name: 'stack',
          component: () => import('./views/StackView.vue'),
          meta: { cap: Cap.StacksView },
        },

        // Deployments are addressed by their own id rather than nested under their stack, so a
        // deployment has ONE canonical URL — the thing you can paste into a message. Both the
        // cross-stack feed and a stack's own history link here.
        {
          path: 'deployments',
          name: 'deployments',
          component: () => import('./views/DeploymentsView.vue'),
          meta: { cap: Cap.StacksView },
        },
        {
          path: 'deployments/:id',
          name: 'deployment',
          component: () => import('./views/DeploymentView.vue'),
          meta: { cap: Cap.StacksView },
        },
        {
          path: 'volume-sources',
          name: 'volume-sources',
          component: () => import('./views/VolumeSourcesView.vue'),
          meta: { cap: Cap.VolsourcesView },
        },
        {
          path: 'images',
          name: 'images',
          component: () => import('./views/ImagesView.vue'),
          meta: { cap: Cap.ImagesView },
        },
        {
          path: 'volumes',
          name: 'volumes',
          component: () => import('./views/VolumesView.vue'),
          meta: { cap: Cap.VolumesView },
        },
        {
          path: 'networks',
          name: 'networks',
          component: () => import('./views/NetworksView.vue'),
          meta: { cap: Cap.NetworksView },
        },
        {
          path: 'cluster',
          name: 'cluster',
          component: () => import('./views/ClusterView.vue'),
          meta: { cap: Cap.ClustersView },
        },
        {
          path: 'backups',
          name: 'backups',
          component: () => import('./views/BackupsView.vue'),
          meta: { cap: Cap.BackupsView },
        },
        {
          path: 'audit',
          name: 'audit',
          component: () => import('./views/AuditView.vue'),
          meta: { cap: Cap.AuditView },
        },

        // Settings: configure-once things, out of the daily navigation. Each tab is
        // gated on its own capability rather than a blanket "admin" — that is the whole
        // point of the model, and a Settings section that were all-or-nothing would give
        // it away.
        {
          path: 'settings',
          component: () => import('./views/SettingsView.vue'),
          children: [
            { path: '', redirect: { name: 'settings-users' } },
            {
              path: 'users',
              name: 'settings-users',
              component: () => import('./views/UsersView.vue'),
              meta: { cap: Cap.UsersView },
            },
            {
              path: 'roles',
              name: 'settings-roles',
              component: () => import('./views/RolesView.vue'),
              meta: { cap: Cap.RolesView },
            },
            {
              path: 'auth',
              name: 'settings-auth',
              component: () => import('./views/AuthenticationView.vue'),
              meta: { cap: Cap.SettingsView },
            },
            {
              path: 'notifications',
              name: 'settings-notifications',
              component: () => import('./views/NotificationsView.vue'),
              meta: { cap: Cap.SettingsView },
            },
            {
              path: 'monitors',
              name: 'settings-monitors',
              component: () => import('./views/MonitorsView.vue'),
              meta: { cap: Cap.MonitorsView },
            },
            {
              path: 'logging',
              name: 'settings-logging',
              component: () => import('./views/LoggingSettingsView.vue'),
              meta: { cap: Cap.LoggingView },
            },
            {
              path: 'clusters',
              name: 'settings-clusters',
              component: () => import('./views/ClustersView.vue'),
              meta: { cap: Cap.ClustersEdit },
            },
            {
              path: 'git',
              name: 'settings-git',
              component: () => import('./views/GitCredentialsView.vue'),
              meta: { cap: Cap.GitCredsView },
            },
            {
              path: 'ssh-keys',
              name: 'settings-ssh',
              component: () => import('./views/SSHKeysView.vue'),
              meta: { cap: Cap.SshkeysView },
            },
            {
              path: 'registries',
              name: 'settings-registries',
              component: () => import('./views/RegistriesView.vue'),
              meta: { cap: Cap.RegistriesView },
            },
            {
              path: 'storage',
              name: 'settings-storage',
              component: () => import('./views/StorageView.vue'),
              meta: { cap: Cap.StorageView },
            },
            {
              path: 'certificates',
              name: 'settings-certificates',
              component: () => import('./views/CertificatesView.vue'),
              meta: { cap: Cap.CertsView },
            },
            {
              path: 'keyrings',
              name: 'settings-keyrings',
              component: () => import('./views/KeyringsView.vue'),
              meta: { cap: Cap.KeyringsView },
            },
          ],
        },
        // No meta.cap: your own tokens are a property of being signed in, like /me.
        {
          path: '/tokens',
          name: 'tokens',
          component: () => import('./views/TokensView.vue'),
        },
        // Catch-all: any path under the shell that matches nothing above. A CHILD of the shell so a
        // 404 keeps the rail and switcher — the reader is one click from a real page, not stranded.
        // No meta.cap (being lost is not a permission); the guard still bounces a signed-out visitor
        // to /login first, which is the right precedence. Kept last so it is the lowest-priority match.
        {
          path: ':pathMatch(.*)*',
          name: 'not-found',
          component: () => import('./views/NotFoundView.vue'),
        },
      ],
    },
  ],
})

router.beforeEach(async (to) => {
  const session = useSession()
  await session.ensureLoaded()

  if (!to.meta.public && !session.user) return { name: 'login' }
  if (to.name === 'login' && session.user) return { name: 'overview' }

  // Landing somewhere they cannot read is a page of 403s, which reads as a broken app
  // rather than as a permission they do not have.
  //
  // canAnywhere, not can: at the moment the guard runs, the host switcher may not have
  // settled on a host yet — and a page IS worth showing if they can use it on any host.
  // The page's own contents are then gated per host by session.can().
  if (to.meta.cap && !session.canAnywhere(to.meta.cap)) {
    return { name: landingFor(session) }
  }
  return true
})

// afterEach, not beforeEach: a guard that redirects must not leave the tab named after the page
// it refused to show you.
//
// This sets the SECTION. A detail view — a stack, a container, a deployment — then names the
// thing itself once it has loaded it, because at navigation time nobody knows yet what it is
// called. So the tab reads "Stacks" for an instant and then "api-gateway · Stacks", rather than
// reading "Daffa" and staying there.
router.afterEach((to) => setTitleForRoute(to.name))

// Where to send someone who has no business on the page they asked for.
//
// Stacks is the usual home, but a role that cannot see stacks would bounce off it and
// straight back here — so pick the first thing they CAN see. If they can see nothing at
// all, send them somewhere that says so, rather than redirecting them in a circle until
// the router gives up.
function landingFor(session: ReturnType<typeof useSession>): string {
  const candidates: [CapValue, string][] = [
    [Cap.StacksView, 'overview'],
    [Cap.StacksView, 'stacks'],
    [Cap.VolsourcesView, 'volume-sources'],
    [Cap.ContainersView, 'containers'],
    [Cap.BackupsView, 'backups'],
    [Cap.ImagesView, 'images'],
    [Cap.VolumesView, 'volumes'],
    [Cap.NetworksView, 'networks'],
    [Cap.ClustersView, 'cluster'],
    [Cap.AuditView, 'audit'],
    [Cap.UsersView, 'settings-users'],
    [Cap.RolesView, 'settings-roles'],
    [Cap.SettingsView, 'settings-auth'],
    [Cap.MonitorsView, 'settings-monitors'],
    [Cap.LoggingView, 'settings-logging'],
    [Cap.GitCredsView, 'settings-git'],
    [Cap.RegistriesView, 'settings-registries'],
    [Cap.StorageView, 'settings-storage'],
    [Cap.CertsView, 'settings-certificates'],
  ]
  for (const [cap, name] of candidates) {
    if (session.canAnywhere(cap)) return name
  }
  return 'no-access'
}
