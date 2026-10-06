# Sourced by dev/cloud/run-cloud-tests.sh and dev/cloud/test/bootstrap.sh: the
# one list of OVMF firmware paths the hosted boot lane accepts (#575).
#
# OVMF is picked as a pair: a CODE image and the VARS template that matches it
# (the sizes must match). The VARS store is required. Without a writable one,
# OVMF saves its variables to an NvVars file on the ESP at every boot, and
# that write loses GRUB's grubenv save (run-cloud-tests.sh has the details).
#
# ovmf_find sets OVMF_CODE and OVMF_VARS_TEMPLATE to the first pair it can read,
# and returns 1 when there is none.
ovmf_find() {
    local pair
    OVMF_CODE=""
    OVMF_VARS_TEMPLATE=""
    for pair in /usr/share/OVMF/OVMF_CODE_4M.fd:/usr/share/OVMF/OVMF_VARS_4M.fd \
                /usr/share/OVMF/OVMF_CODE.fd:/usr/share/OVMF/OVMF_VARS.fd \
                /usr/share/edk2/x64/OVMF_CODE.4m.fd:/usr/share/edk2/x64/OVMF_VARS.4m.fd \
                /usr/share/edk2/ovmf/OVMF_CODE.fd:/usr/share/edk2/ovmf/OVMF_VARS.fd \
                /usr/share/edk2-ovmf/x64/OVMF_CODE.fd:/usr/share/edk2-ovmf/x64/OVMF_VARS.fd; do
        if [ -r "${pair%%:*}" ] && [ -r "${pair#*:}" ]; then
            OVMF_CODE="${pair%%:*}"
            OVMF_VARS_TEMPLATE="${pair#*:}"
            return 0
        fi
    done
    return 1
}

# ovmf_wanted: true when the firmware list (MOOSE_CLOUD_FIRMWARES, default
# "uefi bios") has UEFI in it. A BIOS-only run never starts OVMF.
ovmf_wanted() {
    case " ${MOOSE_CLOUD_FIRMWARES:-uefi bios} " in *" uefi "*) return 0 ;; *) return 1 ;; esac
}
