<script setup lang="ts">
import type { MountSupport, SessionState } from '~/types/api'

const { t } = useI18n()
const { $api } = useNuxtApp()
const session = useState<SessionState>('local-session')
const { isMobile, name: appName } = useAppInfo()
const { exact } = useFormatting()
const {
  data: support,
  status,
  error,
  refresh
} = useLazyAsyncData('mount-support', () => $api<MountSupport>('/mounts/support'), {
  immediate: !isMobile.value && session.value.flags.canMount
})
</script>

<template>
  <WorkspacePanel :title="t('settings.title')">
    <div class="divide-default max-w-3xl divide-y">
      <section class="space-y-5 pb-8">
        <div>
          <h2 class="text-highlighted font-semibold">
            {{ t('settings.appearance') }}
          </h2>
          <p class="text-muted mt-1 text-sm">
            {{ t('settings.appearanceHint') }}
          </p>
        </div>
        <LocaleControls /><USwitch
          v-model="exact"
          :label="t('settings.sizes')"
          :description="t('settings.sizesHint')"
        />
      </section>
      <section class="space-y-4 py-8">
        <div>
          <h2 class="text-highlighted font-semibold">
            {{ t(isMobile ? 'settings.mobile' : 'settings.desktop') }}
          </h2>
          <p class="text-muted mt-1 text-sm">
            {{ t(isMobile ? 'settings.mobileHint' : 'settings.desktopHint') }}
          </p>
        </div>
        <p v-if="!isMobile && !session.tray" class="text-muted text-sm">
          {{ t('app.noTray') }}
        </p>
        <p class="text-muted text-sm">
          {{ t(isMobile ? 'settings.accountStorageMobile' : 'settings.accountStorage') }}
        </p>
      </section>
      <section v-if="!isMobile && session.flags.canMount" class="space-y-4 py-8">
        <h2 class="text-highlighted font-semibold">
          {{ t('settings.driver') }}
        </h2>
        <UProgress
          v-if="status === 'pending' || status === 'idle'"
          :aria-label="t('common.loading')"
        />
        <template v-else-if="error">
          <OperationError :error="error" />
          <UButton
            icon="i-lucide-refresh-cw"
            color="neutral"
            variant="outline"
            :label="t('common.refresh')"
            @click="refresh()"
          />
        </template>
        <UBadge
          v-else-if="support"
          :label="t(support?.available ? 'settings.driverReady' : 'settings.driverMissing')"
          :color="support?.available ? 'success' : 'warning'"
          variant="subtle"
        />
        <MountDriverInfo v-if="support && !support.available" :support="support" />
      </section>
      <section class="space-y-4 py-8">
        <h2 class="text-highlighted font-semibold">
          {{ t('settings.about') }}
        </h2>
        <div class="flex items-center gap-3">
          <img src="/icon.png" :alt="appName" class="size-12 rounded-xl" />
          <div>
            <p class="font-medium">
              {{ appName }}
            </p>
            <p class="text-muted text-sm">
              {{ session.version }}
            </p>
          </div>
        </div>
        <UButton
          to="https://github.com/unofficialpku/disk"
          target="_blank"
          rel="noreferrer"
          icon="i-lucide-external-link"
          :label="t('settings.website')"
          color="neutral"
          variant="link"
          class="px-0"
        />
      </section>
    </div>
  </WorkspacePanel>
</template>
