<script setup lang="ts">
// Shared "are you sure?" pop-up: a title, a short body in the default slot, and
// Cancel / confirm buttons. It is a native <dialog> opened with showModal(), so
// the page behind it is blocked, focus moves into it, and Esc cancels it, all
// from the browser. The caller opens it through a template ref:
//
//   <ConfirmDialog ref="confirm" title="Delete it?" confirm-label="Delete" @confirm="remove">
//     <p>This cannot be undone.</p>
//   </ConfirmDialog>
//   confirm.value?.open()
//
// The confirm button closes the dialog before it emits `confirm`, so the
// handler runs with the dialog already gone. Cancel and Esc just close it and
// emit nothing.
import { ref, useId } from "vue";
import Button from "@/components/ui/Button.vue";

withDefaults(defineProps<{ title: string; confirmLabel?: string; cancelLabel?: string }>(), {
  confirmLabel: "Confirm",
  cancelLabel: "Cancel",
});
const emit = defineEmits<{ confirm: [] }>();

const dialog = ref<HTMLDialogElement | null>(null);
const titleId = useId();

function open() {
  dialog.value?.showModal();
}

function close() {
  dialog.value?.close();
}

function confirm() {
  close();
  emit("confirm");
}

defineExpose({ open, close });
</script>

<template>
  <dialog
    ref="dialog"
    :aria-labelledby="titleId"
    class="m-auto w-[calc(100%-2rem)] max-w-md rounded-xl border border-border bg-card p-0 text-foreground backdrop:bg-black/50"
  >
    <div class="space-y-4 px-5 py-5">
      <h3 :id="titleId" class="text-base font-medium">{{ title }}</h3>
      <div class="space-y-2 text-sm text-muted-foreground">
        <slot />
      </div>
      <div class="flex justify-end gap-2">
        <Button type="button" variant="secondary" @click="close">{{ cancelLabel }}</Button>
        <Button type="button" @click="confirm">{{ confirmLabel }}</Button>
      </div>
    </div>
  </dialog>
</template>
