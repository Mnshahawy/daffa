import { Cap, type CapValue } from './caps'
import type { IconName } from './icons'

/**
 * The navigation, as data.
 *
 * One declaration, read by three things that must never disagree: the rail, the command
 * palette, and the router's "where do I send someone who can't see the page they asked for"
 * fallback. Dokploy keeps its nav in a 1,222-line component and its command palette
 * elsewhere, so the two drifted.
 *
 * Each item names the capability it needs and the rail renders only what this person can
 * actually open — a menu full of links that bounce you back where you came from is worse
 * than a short menu.
 *
 * On the grouping: the old nav was four top-level links with Containers, Images, Volumes,
 * Networks and Host all folded into one dropdown called "Docker" — which put the single
 * most-visited page in the product (Containers) two clicks and a hover away. Dokploy's
 * answer is 28 flat links in one scrolling rail. Neither is right.
 *
 * These four groups are the actual shape of the job:
 *
 *   Deploy     what you came here to do
 *   Operate    what is running right now, and what protects it
 *   Resources  the machinery underneath — you go here to inspect and reclaim, not daily
 *   Records    what happened
 */
export interface NavItem {
  name: string
  label: string
  icon: IconName
  cap: CapValue
  /** What this page IS, for the command palette. One line, plain. */
  hint: string
  /** Extra words someone might search for it by. "vm", "machine" → Host. */
  keywords?: string
}

export interface NavGroup {
  /** Absent for the top group: "Overview" is not a category, it is the front door. */
  title?: string
  items: NavItem[]
}

export const navGroups: NavGroup[] = [
  {
    items: [
      {
        name: 'overview',
        label: 'Overview',
        icon: 'compass',
        cap: Cap.StacksView,
        hint: 'Everything, at a glance',
        keywords: 'home dashboard start bridge',
      },
    ],
  },
  {
    title: 'Deploy',
    items: [
      {
        name: 'stacks',
        label: 'Stacks',
        icon: 'layers',
        cap: Cap.StacksView,
        hint: 'Compose projects and their sources',
        keywords: 'compose project git',
      },
      {
        name: 'deployments',
        label: 'Deployments',
        icon: 'history',
        cap: Cap.StacksView,
        hint: 'Every deploy attempt, newest first',
        keywords: 'deploys releases history rollback',
      },
      // Deploy furniture, not Docker furniture: a source shares git credentials and stack
      // adjacency, so it lives here beside Stacks rather than under Resources with Volumes.
      {
        name: 'volume-sources',
        label: 'Volume sources',
        icon: 'file',
        cap: Cap.VolsourcesView,
        hint: 'Named volumes kept in sync from a git subtree — config, not data',
        keywords: 'git config sync traefik provisioning volume webhook',
      },
    ],
  },
  {
    title: 'Operate',
    items: [
      {
        name: 'containers',
        label: 'Containers',
        icon: 'box',
        cap: Cap.ContainersView,
        hint: 'What is running, and its logs and shell',
        keywords: 'logs exec shell ps',
      },
      // Services sits next to Containers rather than in a "Cluster" section of its own. The
      // navigation must not fork by environment kind, or every page has to explain which half of
      // the app it lives in. It is simply absent when the environment is not a Swarm — which the
      // capability cannot express, so AppShell hides it. See swarmOnly.
      {
        name: 'services',
        label: 'Services',
        icon: 'layers',
        cap: Cap.ServicesView,
        hint: 'What the Swarm is running, and why a task will not start',
        keywords: 'swarm service task replica scale',
      },
      {
        name: 'backups',
        label: 'Backups',
        icon: 'archive',
        cap: Cap.BackupsView,
        hint: 'Scheduled database backups and their snapshots',
        keywords: 'dump snapshot restore s3',
      },
    ],
  },
  {
    title: 'Resources',
    items: [
      {
        name: 'images',
        label: 'Images',
        icon: 'disc',
        cap: Cap.ImagesView,
        hint: 'Pulled images and what still depends on them',
        keywords: 'tags registry pull',
      },
      {
        name: 'volumes',
        label: 'Volumes',
        icon: 'database',
        cap: Cap.VolumesView,
        hint: 'Persistent data, and what is using it',
        keywords: 'storage disk mount',
      },
      {
        name: 'networks',
        label: 'Networks',
        icon: 'network',
        cap: Cap.NetworksView,
        hint: 'Docker networks and their attachments',
        keywords: 'bridge overlay dns',
      },
      {
        name: 'host',
        label: 'Host',
        icon: 'server',
        cap: Cap.HostsView,
        hint: 'The daemon, its version and its disk',
        keywords: 'daemon machine server info df disk',
      },
    ],
  },
  {
    title: 'Records',
    items: [
      {
        name: 'audit',
        label: 'Audit',
        icon: 'scroll',
        cap: Cap.AuditView,
        hint: 'Every mutating action, and every one refused',
        keywords: 'log who did what history security',
      },
    ],
  },
]

/**
 * Settings, grouped.
 *
 * Nine flat tabs was a list, not a structure — "Users" and "Storage" sat side by side as if
 * they were the same kind of decision. These three groups are the three questions Settings
 * actually answers: who gets in, what Daffa can reach, and when it should speak up.
 */
export interface SettingsTab {
  name: string
  label: string
  hint: string
  cap: CapValue
  icon: IconName
}

export const settingsGroups: { title: string; items: SettingsTab[] }[] = [
  {
    title: 'Access',
    items: [
      {
        name: 'settings-users',
        label: 'Users',
        hint: 'Who can sign in',
        cap: Cap.UsersView,
        icon: 'users',
      },
      {
        name: 'settings-roles',
        label: 'Roles',
        hint: 'What they are allowed to do',
        cap: Cap.RolesView,
        icon: 'check',
      },
      {
        name: 'settings-auth',
        label: 'Authentication',
        hint: 'Passwords and identity providers',
        cap: Cap.SettingsView,
        icon: 'plug',
      },
    ],
  },
  {
    title: 'Connections',
    items: [
      {
        name: 'settings-agents',
        label: 'Hosts',
        hint: 'The machines Daffa manages',
        cap: Cap.HostsEdit,
        icon: 'server',
      },
      {
        name: 'settings-git',
        label: 'Git',
        hint: 'Access to your repositories',
        cap: Cap.GitCredsView,
        icon: 'layers',
      },
      {
        name: 'settings-registries',
        label: 'Registries',
        hint: 'Credentials for private images',
        cap: Cap.RegistriesView,
        icon: 'disc',
      },
      {
        name: 'settings-storage',
        label: 'Storage',
        hint: 'S3-compatible buckets for backups',
        cap: Cap.StorageView,
        icon: 'database',
      },
      {
        name: 'settings-certificates',
        label: 'Certificates',
        hint: 'Internal CAs, the certs they sign, backup encryption keys',
        cap: Cap.CertsView,
        icon: 'shield',
      },
      {
        name: 'settings-keyrings',
        label: 'Keyrings',
        hint: 'Rotatable app encryption keys, delivered to hosts in volumes',
        cap: Cap.KeyringsView,
        icon: 'key',
      },
    ],
  },
  {
    title: 'Alerts',
    items: [
      {
        name: 'settings-monitors',
        label: 'Resource monitors',
        hint: 'Alert when a container stays over the line',
        cap: Cap.MonitorsView,
        icon: 'gauge',
      },
      {
        name: 'settings-logging',
        label: 'Container logs',
        hint: 'Default log driver and rotation for deployed stacks',
        cap: Cap.LoggingView,
        icon: 'scroll',
      },
      {
        name: 'settings-notifications',
        label: 'Notifications',
        hint: 'Who gets told, and when',
        cap: Cap.SettingsView,
        icon: 'inbox',
      },
    ],
  },
]

/** Flattened, for the palette and for the router's landing fallback. */
export const allNavItems: NavItem[] = navGroups.flatMap((g) => g.items)
export const allSettingsTabs: SettingsTab[] = settingsGroups.flatMap((g) => g.items)
