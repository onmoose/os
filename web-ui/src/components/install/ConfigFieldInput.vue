<script setup lang="ts">
// One user-supplied setup field (APP_MANIFEST.md # D4) on the install setup
// page. Each field is the app's own env var, so the app_env shows as a small
// monospace hint under the help text: a user reading the app's own docs can
// match it (DASHBOARD.md # Install authorization).
//
// The markup is ported from Tailwind Plus: input with label and help text for
// text and secret, the simple native select for enum, and the toggle with a
// left label and description for bool. The value is always a string, the way
// the brain takes it: a bool travels as "true" or "false".
import { computed } from "vue";
import { ChevronDown } from "lucide-vue-next";
import type { InstallPlanConfigField } from "../../api";

const props = defineProps<{ field: InstallPlanConfigField }>();
const model = defineModel<string>({ required: true });

const id = computed(() => `config-${props.field.app_env}`);
const helpId = computed(() => `${id.value}-help`);

const inputClass =
  "block w-full rounded-md bg-card px-3 py-1.5 text-base text-foreground outline-1 -outline-offset-1 " +
  "outline-border placeholder:text-muted-foreground focus:outline-2 focus:-outline-offset-2 " +
  "focus:outline-accent sm:text-sm/6";
</script>

<template>
  <!-- bool: the label sits left of the switch, so it reads as one line. -->
  <div v-if="field.type === 'bool'" class="flex items-center justify-between gap-4">
    <span class="flex grow flex-col">
      <label :id="`${id}-label`" :for="id" class="text-sm/6 font-medium text-foreground">{{ field.title }}</label>
      <span :id="helpId" class="text-sm text-muted-foreground">{{ field.description }}</span>
      <span class="mt-0.5 font-mono text-xs text-muted-foreground">Sets {{ field.app_env }}</span>
    </span>
    <div
      class="group relative inline-flex w-11 shrink-0 rounded-full bg-olive-200 p-0.5 inset-ring inset-ring-olive-950/5 outline-offset-2 outline-accent transition-colors duration-200 ease-in-out has-checked:bg-accent has-focus-visible:outline-2"
    >
      <span
        class="size-5 rounded-full bg-card shadow-xs ring-1 ring-olive-950/5 transition-transform duration-200 ease-in-out group-has-checked:translate-x-5"
      />
      <input
        :id="id"
        type="checkbox"
        :checked="model === 'true'"
        :aria-labelledby="`${id}-label`"
        :aria-describedby="helpId"
        class="absolute inset-0 size-full cursor-pointer appearance-none focus:outline-hidden"
        @change="model = ($event.target as HTMLInputElement).checked ? 'true' : 'false'"
      />
    </div>
  </div>

  <div v-else>
    <label :for="id" class="block text-sm/6 font-medium text-foreground">
      {{ field.title }}<span v-if="field.required" class="text-destructive"> *</span>
    </label>
    <div class="mt-2 grid grid-cols-1">
      <template v-if="field.type === 'enum'">
        <select
          :id="id"
          v-model="model"
          :aria-describedby="helpId"
          :class="[inputClass, 'col-start-1 row-start-1 appearance-none pr-8']"
        >
          <!-- A required enum with no default starts unset. The disabled
               placeholder keeps what is shown equal to the empty value, so the
               Install gate stays honest until the user picks. -->
          <option v-if="!field.required" value="">None</option>
          <option v-else value="" disabled>Select…</option>
          <option v-for="opt in field.options ?? []" :key="opt" :value="opt">{{ opt }}</option>
        </select>
        <ChevronDown
          class="pointer-events-none col-start-1 row-start-1 mr-2 size-4 self-center justify-self-end text-muted-foreground"
          aria-hidden="true"
        />
      </template>
      <input
        v-else
        :id="id"
        v-model="model"
        :type="field.secret ? 'password' : 'text'"
        :placeholder="field.secret ? '' : (field.default ?? '')"
        :aria-describedby="helpId"
        :autocomplete="field.secret ? 'new-password' : 'off'"
        :class="inputClass"
      />
    </div>
    <p :id="helpId" class="mt-2 text-sm text-muted-foreground">{{ field.description }}</p>
    <p class="mt-0.5 font-mono text-xs text-muted-foreground">Sets {{ field.app_env }}</p>
  </div>
</template>
